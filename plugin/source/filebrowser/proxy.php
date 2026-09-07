<?php
/* proxy.php - authenticated bridge between the webGUI and filebrowserd
 *
 * Copyright 2026, James Smyth
 *
 * Licensed under the MIT License; see the LICENSE file in the project root.
 *
 * ---------------------------------------------------------------------------
 * SECURITY NOTES - read before changing anything in this file.
 * ---------------------------------------------------------------------------
 *
 * The daemon binds only /var/run/filebrowserd.sock (root-owned, mode 0600) and
 * never a TCP port. This script is the ONLY way into it, and it lives inside
 * the emhttp document root, so every request that reaches it has already
 * passed Unraid's login (nginx auth_request -> /auth-request.php) exactly like
 * any other webGUI page. There is no second token, no CORS surface and no
 * listener on the LAN.
 *
 *   Usage:  proxy.php?p=<urlencoded API path, including its query string>
 *   e.g.    proxy.php?p=%2Fapi%2Fv1%2Ffs%2Flist%3Fpath%3D%2Fmnt%2Fuser
 *
 * Rules enforced here:
 *   - `p` must match ^/api/v1/[A-Za-z0-9/._-]*(\?.*)?$ and consist only of
 *     printable ASCII with no space (\x21-\x7e) - nothing else can be reached,
 *     in particular no absolute URL, no other daemon route and no
 *     scheme/authority that could turn this into an SSRF gadget. The SPA always
 *     percent-encodes, so nothing legitimate needs a raw space or a control
 *     byte.
 *   - CR/LF anywhere in `p` or in a forwarded header value is rejected, so a
 *     crafted query string cannot smuggle a second request line into the
 *     socket (HTTP request splitting).
 *   - only GET and POST are accepted. POST may carry X-HTTP-Method-Override:
 *     PUT and nothing else; the override is validated *before* we open the
 *     socket, so a rejected request never touches the daemon.
 *   - a POST body must be application/json. The SPA sends nothing else, and
 *     refusing form/multipart bodies keeps this endpoint off the list of
 *     things a cross-origin <form> could ever post to.
 *   - the webGUI csrf_token is verified here as well, independently of
 *     Unraid's auto_prepend (see CSRF below).
 *   - only Range / Accept / Content-Type / Content-Length are forwarded from
 *     the client. Cookies, Authorization, X-Forwarded-*, Accept-Encoding and
 *     everything else are dropped: the daemon must never see webGUI session
 *     material, and dropping Accept-Encoding keeps the body relay simple.
 *   - only Content-Type / Content-Length / Content-Disposition / Content-Range
 *     / Accept-Ranges / Cache-Control / Content-Security-Policy /
 *     X-Content-Type-Options are relayed back. Upstream Set-Cookie or
 *     redirects can never reach the browser.
 *   - fs/raw responses are additionally hardened here, independently of what
 *     the daemon sent (see "raw hardening" below): the webGUI origin is the
 *     root-authenticated origin, so a file served inline with an attacker
 *     chosen Content-Type would be stored XSS with root privileges.
 *   - NOTHING in this file shells out. No exec/system/popen/proc_open, no
 *     backticks. Path strings from the browser are only ever written into an
 *     HTTP request line on a unix socket.
 *
 * CSRF: Unraid enforces its csrf_token on every POST from
 * /usr/local/emhttp/plugins/dynamix/include/local_prepend.php (an
 * auto_prepend_file), before this script runs. We do NOT rely on that alone -
 * a misconfigured or missing prepend would otherwise silently disable the only
 * CSRF control on a root-privileged endpoint - so the token is compared here
 * too, against csrf_token in /var/local/emhttp/var.ini, with hash_equals().
 * The SPA sends it in the `X-CSRF-TOKEN` header so that the request body stays
 * byte-identical for the daemon (local_prepend strips a csrf_token POST *field*
 * from $_POST but not from php://input, which would desync Content-Length).
 * The token is handed to the SPA by FileBrowser.page on the iframe URL. GETs
 * are unaffected.
 */

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------
$FB_SOCKET       = '/var/run/filebrowserd.sock';
$FB_VAR_INI      = '/var/local/emhttp/var.ini';
$FB_CONNECT_WAIT = 5;      // seconds to wait for connect()
$FB_TIMEOUT      = 60;     // seconds of read inactivity for normal requests
$FB_CHUNK        = 65536;  // body relay buffer

// SSE budgets. A php-fpm worker is pinned for the whole life of an event
// stream, so an abandoned tab must never hold one: we poll the socket, ping
// the client, and give up on our own schedule. EventSource reconnects
// transparently and the SPA also has a polling fallback.
// The ping interval is deliberately short: a closed client is only detected on
// the *second* write after it went away (the first write is what makes the
// peer's kernel send the reset), so worst-case detection is two intervals.
$FB_SSE_SELECT   = 5;      // stream_select() timeout - also the ping interval
$FB_SSE_SILENCE  = 60;     // give up if the daemon says nothing for this long
$FB_SSE_MAXLIFE  = 600;    // hard cap on one stream; the browser reconnects

// Path grammar. The character class permits '.', so '..' is rejected
// separately below - the daemon does its own confinement, this is defence in
// depth for the transport.
$FB_PATH_RE  = '#^/api/v1/[A-Za-z0-9/._-]*(\?.*)?$#D';
// Every byte of `p` must be printable ASCII with no space.
$FB_PRINT_RE = '#^[\x21-\x7e]+$#D';

// Content types fs/raw may be served with as-is. Everything else is downgraded
// to an attachment download, so the browser never renders daemon-supplied
// bytes as a document on the webGUI origin.
$FB_RAW_INLINE_TYPES = [
  'image/png', 'image/jpeg', 'image/gif', 'image/webp', 'image/bmp',
  'image/avif', 'application/pdf', 'text/plain',
];
$FB_RAW_INLINE_PREFIXES = ['audio/', 'video/'];

// PHP appends ";charset=<default_charset>" to any text/* Content-Type it emits.
// The daemon already decides the charset (fs/view converts to UTF-8, fs/raw
// reports what it sniffed), so clear it and relay Content-Type verbatim.
@ini_set('default_charset', '');

// ---------------------------------------------------------------------------
// error envelope - matches the daemon's own {ok:false,error:{code,message}}
// ---------------------------------------------------------------------------
function fb_fail($status, $code, $message) {
  if (!headers_sent()) {
    http_response_code($status);
    header('Content-Type: application/json');
    header('Cache-Control: no-store');
  }
  echo json_encode(['ok' => false, 'error' => ['code' => $code, 'message' => $message]]);
  exit;
}

// A header value that survives to the socket must not be able to break framing.
function fb_clean_header($value) {
  $value = (string)$value;
  if (strpbrk($value, "\r\n") !== false) return null;
  $value = trim($value);
  return ($value === '') ? null : $value;
}

/** The webGUI's current csrf_token, or null when it cannot be determined. */
function fb_expected_csrf($file) {
  if (is_readable($file)) {
    $ini = @parse_ini_file($file, false, INI_SCANNER_RAW);
    if (is_array($ini) && isset($ini['csrf_token']) && is_string($ini['csrf_token'])) {
      $tok = trim($ini['csrf_token'], " \t\"'");
      if ($tok !== '') return $tok;
    }
    // var.ini occasionally carries keys parse_ini_file dislikes; fall back to
    // reading just the one line we need.
    $raw = @file_get_contents($file);
    if (is_string($raw) && preg_match('/^csrf_token\s*=\s*"?([^"\r\n]*)"?/m', $raw, $m)) {
      $tok = trim($m[1]);
      if ($tok !== '') return $tok;
    }
  }
  return null;
}

/** Content-Type without parameters, lowercased. */
function fb_mime_only($ctype) {
  $semi = strpos($ctype, ';');
  if ($semi !== false) $ctype = substr($ctype, 0, $semi);
  return strtolower(trim($ctype));
}

/**
 * A filename we are willing to put in our own Content-Disposition, taken from
 * the upstream header. Anything that could break the header (quotes,
 * backslashes, CR/LF, control bytes) disqualifies it and we fall back to a
 * bare `attachment`.
 */
function fb_attachment_disposition($upstreamCd) {
  $name = '';
  if (is_string($upstreamCd) && $upstreamCd !== '') {
    if (preg_match('/filename\s*=\s*"([^"\\\\\r\n]*)"/i', $upstreamCd, $m)) {
      $name = $m[1];
    } elseif (preg_match('/filename\s*=\s*([^";,\r\n]+)/i', $upstreamCd, $m)) {
      $name = trim($m[1]);
    }
  }
  // Printable ASCII only, no path separators, no quoting characters.
  if ($name === '' || strlen($name) > 200) return 'attachment';
  if (!preg_match('#^[\x20-\x7e]+$#D', $name)) return 'attachment';
  if (strpbrk($name, "\"\\/") !== false) return 'attachment';
  return 'attachment; filename="' . $name . '"';
}

// ---------------------------------------------------------------------------
// validate the request  (everything here happens BEFORE we open the socket)
// ---------------------------------------------------------------------------
$method = $_SERVER['REQUEST_METHOD'] ?? 'GET';
if ($method !== 'GET' && $method !== 'POST') {
  header('Allow: GET, POST');
  fb_fail(405, 'BAD_REQUEST', 'only GET and POST are supported');
}

$p = $_GET['p'] ?? '';
if (!is_string($p) || $p === '') {
  fb_fail(400, 'BAD_REQUEST', 'missing p parameter');
}
if (strpbrk($p, "\r\n") !== false) {
  fb_fail(400, 'BAD_REQUEST', 'illegal characters in p');
}
if (!preg_match($FB_PRINT_RE, $p)) {
  fb_fail(400, 'BAD_REQUEST', 'p must be printable ASCII with no spaces');
}
if (!preg_match($FB_PATH_RE, $p)) {
  fb_fail(400, 'BAD_REQUEST', 'p must be an /api/v1 path');
}

$qpos     = strpos($p, '?');
$pathOnly = ($qpos === false) ? $p : substr($p, 0, $qpos);
if (strpos($pathOnly, '..') !== false) {
  fb_fail(400, 'BAD_REQUEST', 'path traversal is not allowed in p');
}

// The SPA tunnels PUT (/index/config) as POST + X-HTTP-Method-Override so the
// browser-facing method stays POST and the CSRF check (POST-only) always
// applies. Only PUT is translatable; the override never reaches the daemon as
// a header, only as the upstream request method.
$upstreamMethod = $method;
$ctype = null;
if ($method === 'POST') {
  $override = fb_clean_header($_SERVER['HTTP_X_HTTP_METHOD_OVERRIDE'] ?? '');
  if ($override !== null) {
    if (strtoupper($override) !== 'PUT') {
      fb_fail(400, 'BAD_REQUEST', 'unsupported method override');
    }
    $upstreamMethod = 'PUT';
  }

  // Body content type: JSON only. The SPA never sends anything else, and a
  // cross-origin <form> cannot produce application/json without a preflight.
  $ctype = fb_clean_header($_SERVER['CONTENT_TYPE'] ?? '');
  if ($ctype === null || fb_mime_only($ctype) !== 'application/json') {
    fb_fail(415, 'BAD_REQUEST', 'POST body must be application/json');
  }

  // CSRF, verified here and not only in Unraid's auto_prepend.
  $expected = fb_expected_csrf($FB_VAR_INI);
  $provided = fb_clean_header($_SERVER['HTTP_X_CSRF_TOKEN'] ?? '');
  if ($provided === null && isset($_POST['csrf_token']) && is_string($_POST['csrf_token'])) {
    $provided = fb_clean_header($_POST['csrf_token']);
  }
  if ($expected === null) {
    fb_fail(403, 'FORBIDDEN', 'csrf token unavailable');
  }
  if ($provided === null || !hash_equals($expected, $provided)) {
    fb_fail(403, 'FORBIDDEN', 'invalid or missing csrf token');
  }
}

// ---------------------------------------------------------------------------
// request body (POST only)
// ---------------------------------------------------------------------------
// Only application/json gets this far, so PHP never populated $_POST from the
// body and php://input always holds it verbatim - no rebuild, no Content-Length
// desync, and the csrf_token the SPA sends as a header is not in the body at
// all so the daemon never sees it.
$body = '';
if ($method === 'POST') {
  $body = (string)@file_get_contents('php://input');
}

// ---------------------------------------------------------------------------
// connect
// ---------------------------------------------------------------------------
$errno  = 0;
$errstr = '';
$sock = @stream_socket_client('unix://' . $FB_SOCKET, $errno, $errstr, $FB_CONNECT_WAIT);
if ($sock === false) {
  fb_fail(502, 'INTERNAL', 'daemon not running');
}
stream_set_blocking($sock, true);
stream_set_timeout($sock, $FB_TIMEOUT);

// ---------------------------------------------------------------------------
// build and send the request
// ---------------------------------------------------------------------------
$forward = [];

$range = fb_clean_header($_SERVER['HTTP_RANGE'] ?? '');
if ($range !== null) $forward['Range'] = $range;

$accept = fb_clean_header($_SERVER['HTTP_ACCEPT'] ?? '');
if ($accept !== null) $forward['Accept'] = $accept;

if ($method === 'POST') {
  if ($ctype !== null) $forward['Content-Type'] = $ctype;
  // Always derive Content-Length from the body we are actually going to write.
  // A client-supplied value that disagreed would desync the connection.
  $forward['Content-Length'] = (string)strlen($body);
}

$req  = $upstreamMethod . ' ' . $p . " HTTP/1.1\r\n";
$req .= "Host: localhost\r\n";
$req .= "Connection: close\r\n";
foreach ($forward as $name => $value) {
  $req .= $name . ': ' . $value . "\r\n";
}
$req .= "\r\n";
if ($body !== '') $req .= $body;

if (@fwrite($sock, $req) === false) {
  fclose($sock);
  fb_fail(502, 'INTERNAL', 'daemon not running');
}

// ---------------------------------------------------------------------------
// read the status line and response headers
// ---------------------------------------------------------------------------
$statusLine = @fgets($sock, 8192);
if ($statusLine === false || $statusLine === '') {
  fclose($sock);
  fb_fail(502, 'INTERNAL', 'daemon not running');
}
if (!preg_match('#^HTTP/1\.[01]\s+(\d{3})#', $statusLine, $m)) {
  fclose($sock);
  fb_fail(502, 'INTERNAL', 'malformed response from daemon');
}
$status = (int)$m[1];

$upstream = [];
while (true) {
  $line = @fgets($sock, 16384);
  if ($line === false) {
    fclose($sock);
    fb_fail(502, 'INTERNAL', 'truncated response from daemon');
  }
  $line = rtrim($line, "\r\n");
  if ($line === '') break;
  $colon = strpos($line, ':');
  if ($colon === false) continue;
  $upstream[strtolower(substr($line, 0, $colon))] = trim(substr($line, $colon + 1));
}

$upType   = $upstream['content-type'] ?? '';
$isSse    = (stripos($upType, 'text/event-stream') === 0);
$chunked  = (stripos($upstream['transfer-encoding'] ?? '', 'chunked') !== false);
$hasLen   = array_key_exists('content-length', $upstream) && !$chunked;
$bodyLen  = $hasLen ? (int)$upstream['content-length'] : -1;

// ---------------------------------------------------------------------------
// relay status + whitelisted headers
// ---------------------------------------------------------------------------
http_response_code($status);

$relay = [
  'content-type'            => 'Content-Type',
  'content-disposition'     => 'Content-Disposition',
  'content-range'           => 'Content-Range',
  'accept-ranges'           => 'Accept-Ranges',
  'cache-control'           => 'Cache-Control',
  'content-security-policy' => 'Content-Security-Policy',
  'x-content-type-options'  => 'X-Content-Type-Options',
];
foreach ($relay as $key => $name) {
  if (isset($upstream[$key]) && strpbrk($upstream[$key], "\r\n") === false) {
    header($name . ': ' . $upstream[$key]);
  }
}

// ---- raw hardening --------------------------------------------------------
// Belt and braces with the daemon, which enforces the same rules: a plugin and
// a daemon of different vintages must never combine into "arbitrary file body
// rendered as a document on the root-authenticated webGUI origin". These
// header() calls replace whatever was relayed above.
if ($pathOnly === '/api/v1/fs/raw') {
  header('X-Content-Type-Options: nosniff');
  header("Content-Security-Policy: default-src 'none'; sandbox");

  $mime   = fb_mime_only($upType);
  $inline = in_array($mime, $FB_RAW_INLINE_TYPES, true);
  if (!$inline) {
    foreach ($FB_RAW_INLINE_PREFIXES as $prefix) {
      if (strncmp($mime, $prefix, strlen($prefix)) === 0) { $inline = true; break; }
    }
  }
  if (!$inline) {
    header('Content-Type: application/octet-stream');
    header('Content-Disposition: ' . fb_attachment_disposition($upstream['content-disposition'] ?? ''));
  }
}

// Content-Length only when we relay the body verbatim. Chunked bodies are
// decoded here and emitted plain, so the upstream length would be wrong.
if ($hasLen && !$isSse) {
  header('Content-Length: ' . $bodyLen);
}

// ---------------------------------------------------------------------------
// SSE mode
// ---------------------------------------------------------------------------
// One event stream = one pinned php-fpm worker, so this loop owns its own
// lifetime rather than waiting on the daemon: it polls with stream_select(),
// writes a ping comment on every idle tick (which is what makes
// connection_aborted() true for a closed tab), gives up after
// $FB_SSE_SILENCE of real silence and never runs longer than $FB_SSE_MAXLIFE.
// EventSource reconnects on its own, so the cap is invisible to the user.
if ($isSse) {
  @ini_set('zlib.output_compression', 'Off');
  @ini_set('output_buffering', 'Off');
  @ini_set('implicit_flush', '1');
  while (ob_get_level() > 0) {
    if (!@ob_end_flush()) break;
  }
  ob_implicit_flush(true);
  // nginx sits in front of php-fpm; without this it would buffer the stream.
  header('X-Accel-Buffering: no');
  header('Cache-Control: no-cache');
  header('Connection: keep-alive');
  ignore_user_abort(true);   // we decide when to stop, so the socket is closed
  @set_time_limit(0);
  // Non-blocking for the whole stream: a blocking fread() waits until it can
  // fill its buffer (or the stream timeout expires), which would hold a single
  // 30-byte event back for a full poll interval. stream_select() below is what
  // does the waiting now.
  stream_set_blocking($sock, false);

  // Incremental chunked-transfer decoder: fed arbitrary slabs, emits body
  // bytes. The size line is capped at 8 hex digits so a bogus length cannot
  // turn into an unbounded read.
  $cstate  = 'size';
  $cbuf    = '';
  $cremain = 0;
  $cdone   = false;
  $dechunk = function ($in) use (&$cstate, &$cbuf, &$cremain, &$cdone) {
    $cbuf .= $in;
    $out = '';
    while (!$cdone) {
      if ($cstate === 'size') {
        $nl = strpos($cbuf, "\n");
        if ($nl === false) break;
        $line = trim(substr($cbuf, 0, $nl));
        $cbuf = substr($cbuf, $nl + 1);
        if ($line === '') continue;                  // CRLF between chunks
        $semi = strpos($line, ';');                  // chunk extensions
        if ($semi !== false) $line = rtrim(substr($line, 0, $semi));
        if (!preg_match('/^[0-9A-Fa-f]{1,8}$/D', $line)) { $cdone = true; break; }
        $cremain = hexdec($line);
        if ($cremain === 0) { $cdone = true; break; } // last chunk
        $cstate = 'data';
      } else {
        if ($cbuf === '') break;
        $take    = min($cremain, strlen($cbuf));
        $out    .= substr($cbuf, 0, $take);
        $cbuf    = substr($cbuf, $take);
        $cremain -= $take;
        if ($cremain === 0) $cstate = 'size';
      }
    }
    return $out;
  };

  $deadline = time() + $FB_SSE_MAXLIFE;
  $lastData = time();
  while (true) {
    if (connection_aborted() || $cdone) break;
    $now = time();
    if ($now >= $deadline) break;                     // hard lifetime cap
    if (($now - $lastData) >= $FB_SSE_SILENCE) break; // daemon has gone quiet

    // fgets() while reading the headers may already have pulled the first
    // event into PHP's stream buffer, and stream_select() cannot see that.
    $meta = stream_get_meta_data($sock);
    if (!empty($meta['unread_bytes'])) {
      $ready = 1;
    } else {
      $read   = [$sock];
      $write  = null;
      $except = null;
      $ready  = @stream_select($read, $write, $except, $FB_SSE_SELECT);
      if ($ready === false) break;
    }

    if ($ready === 0) {
      // Idle tick: a comment line is a no-op for EventSource but a real write,
      // which is the only way to learn that the tab is gone.
      echo ": ping\n\n";
      @flush();
      if (connection_aborted()) break;
      continue;
    }

    if (feof($sock)) break;
    $buf = @fread($sock, $FB_CHUNK);
    if ($buf === false || $buf === '') {
      if (feof($sock)) break;
      continue;
    }
    $lastData = time();

    $out = $chunked ? $dechunk($buf) : $buf;
    if ($out !== '') {
      echo $out;
      @flush();
      if (connection_aborted()) break;
    }
  }

  fclose($sock);
  exit;
}

// ---------------------------------------------------------------------------
// body relay
// ---------------------------------------------------------------------------
/** Write one slab to the client. Returns false once the client is gone. */
$emit = function ($data) {
  if ($data === '') return true;
  echo $data;
  // Keeps memory flat on multi-GB fs/raw downloads.
  if (ob_get_level() > 0) @ob_flush();
  @flush();
  return !connection_aborted();
};

/** True when the socket stopped producing because it hit its read timeout. */
$timed_out = function ($sock) {
  $meta = stream_get_meta_data($sock);
  return !empty($meta['timed_out']);
};

if ($status === 204 || $status === 304) {
  // no body

} elseif ($chunked) {
  // ---- chunked transfer coding: decode, emit plain ------------------------
  $stop = false;
  while (!$stop && !feof($sock)) {
    $line = @fgets($sock, 1024);
    if ($line === false) break;
    $line = trim($line);
    if ($line === '') continue;                       // CRLF between chunks
    $semi = strpos($line, ';');                       // chunk extensions
    if ($semi !== false) $line = substr($line, 0, $semi);
    $line = trim($line);
    if ($line === '' || !preg_match('/^[0-9A-Fa-f]{1,8}$/D', $line)) break;
    $remaining = hexdec($line);
    if ($remaining === 0) break;                      // last chunk (trailers ignored)
    while ($remaining > 0) {
      $buf = @fread($sock, min($FB_CHUNK, $remaining));
      if ($buf === false || $buf === '') {            // EOF or read timeout
        $stop = true;
        break;
      }
      $remaining -= strlen($buf);
      if (!$emit($buf)) {                             // client went away
        $stop = true;
        break;
      }
    }
    if ($stop) break;
    @fread($sock, 2);                                 // trailing CRLF
  }

} elseif ($hasLen) {
  // ---- Content-Length framing --------------------------------------------
  $remaining = $bodyLen;
  while ($remaining > 0 && !feof($sock)) {
    $buf = @fread($sock, min($FB_CHUNK, $remaining));
    if ($buf === false || $buf === '') break;
    $remaining -= strlen($buf);
    if (!$emit($buf)) break;
  }

} else {
  // ---- read until the daemon closes (we asked for Connection: close) ------
  while (!feof($sock)) {
    $buf = @fread($sock, $FB_CHUNK);
    if ($buf === false || $buf === '') {
      if ($timed_out($sock)) break;
      if (feof($sock)) break;
      continue;
    }
    if (!$emit($buf)) break;
  }
}

fclose($sock);
