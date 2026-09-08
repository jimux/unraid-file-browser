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
 *   - a POST body is either application/json (the body IS the upstream body) or
 *     the application/x-www-form-urlencoded envelope described under CSRF
 *     below, whose `payload` field carries the JSON. Nothing else - in
 *     particular no multipart, so this endpoint can never be a file drop.
 *     The envelope's only two recognised fields are csrf_token and payload;
 *     any other field is a 400.
 *   - the webGUI csrf_token is verified here as well, independently of
 *     Unraid's auto_prepend, and it is verified FIRST: on a POST nothing else
 *     is parsed, and no socket is opened, until hash_equals() has passed.
 *     Accepting a form-encoded body means a cross-origin <form> can at least
 *     reach the parser, so that check is now load-bearing rather than
 *     belt-and-braces (see CSRF below).
 *   - the bridge never emits a 2xx whose body is not what the daemon sent. A
 *     bounded (non fs/raw, non-SSE) response is read in full and checked
 *     against its own framing before a single byte is committed; if the daemon
 *     truncates it, or if something already started the response before we
 *     could label it, the client gets an {ok:false} 502 envelope instead of a
 *     plausible-looking short 200 (see "commit discipline" below).
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
 *   - fs/raw is relayed as a *stream*: no execution-time limit, no output
 *     buffer, no nginx buffering, and a daemon that goes quiet is waited on
 *     rather than treated as end-of-body (see "streaming preamble" below).
 *     A vanished client still ends it immediately - ignore_user_abort is left
 *     false, unlike the SSE branch.
 *   - NOTHING in this file shells out. No exec/system/popen/proc_open, no
 *     backticks. Path strings from the browser are only ever written into an
 *     HTTP request line on a unix socket.
 *
 * ---------------------------------------------------------------------------
 * CSRF, and why a POST body is form-encoded here
 * ---------------------------------------------------------------------------
 * Unraid enforces its csrf_token on every POST from
 * /usr/local/emhttp/plugins/dynamix/include/local_prepend.php, an
 * auto_prepend_file named in /etc/php/php.ini, which therefore runs before this
 * script on every request. On 7.2.0 it reads the token from ONE place:
 *
 *     if (!isset($_POST['csrf_token'])) csrf_terminate("missing");
 *     ...
 *     function csrf_terminate($reason) { exec('logger ...'); exit; }
 *
 * $_POST is populated by PHP only for form-encoded and multipart bodies, so an
 * application/json POST leaves it empty and every such request was rejected -
 * by an exit() that emits no body and sets no status, which php-fpm turns into
 * a bare 200 with an empty body. That is what the field reported as
 * "unexpected response shape (HTTP 200)": the daemon was never contacted at
 * all. Syslog carries the matching line:
 *
 *     webGUI: error: /plugins/filebrowser/proxy.php?p=... - missing csrf_token
 *
 * So the token has to reach $_POST. Production non-GET requests are therefore
 * sent as
 *
 *     Content-Type: application/x-www-form-urlencoded; charset=UTF-8
 *     csrf_token=<token>&payload=<urlencoded JSON body>
 *
 * and this script forwards the `payload` field to the daemon as a clean
 * application/json body with a Content-Length derived from its actual bytes.
 * The daemon's wire contract is unchanged; the envelope exists purely to get
 * past the prepend.
 *
 * The token is read out of the RAW body (php://input), never out of $_POST,
 * because both prepend generations consume what they validated:
 * 7.2.0 does `unset($_POST['csrf_token'])` and current master additionally does
 * `unset($_SERVER['HTTP_X_CSRF_TOKEN'])`. Reading php://input is the only
 * source neither of them can take away, which is also what makes our own
 * independent hash_equals() check survive a future Unraid that consumes the
 * header. php://input is always readable here because multipart bodies - the
 * one shape PHP does not leave in php://input - are refused outright.
 *
 * We do NOT rely on the prepend alone: a misconfigured or missing one would
 * otherwise silently disable the only CSRF control on a root-privileged
 * endpoint, so the token is compared here too, against csrf_token in
 * /var/local/emhttp/var.ini, with hash_equals(), before anything else on a POST
 * is parsed. The SPA also still sends X-CSRF-TOKEN, which satisfies newer
 * Unraid prepends and is what a plain application/json POST (dev server, curl)
 * uses. The token is handed to the SPA by FileBrowser.page on the iframe URL.
 * GETs are unaffected - the prepend ignores them and so do we.
 *
 * ---------------------------------------------------------------------------
 * Commit discipline
 * ---------------------------------------------------------------------------
 * Everything above is about a response that never got made. The mirror-image
 * failure is a response that got made wrongly, and the bridge used to have no
 * defence against it: http_response_code() and header() are silent no-ops once
 * anything has been written, so a stray byte from an auto_prepend, or a PHP
 * warning, would leave the daemon's 422 relabelled 200 and its Content-Length
 * off by the length of the noise. Nothing downstream can tell.
 *
 * So the response is committed exactly once, at fb_commit(), which refuses to
 * run if output has already begun, and bounded responses are read in full and
 * validated against their own framing first. A body the bridge cannot
 * reproduce faithfully becomes an honest {ok:false} 502 envelope - never a 2xx
 * carrying something other than the daemon's bytes.
 */

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------
$FB_SOCKET       = '/var/run/filebrowserd.sock';
$FB_VAR_INI      = '/var/local/emhttp/var.ini';
$FB_CONNECT_WAIT = 5;      // seconds to wait for connect()
$FB_TIMEOUT      = 60;     // seconds of read inactivity for normal requests
$FB_CHUNK        = 65536;  // body relay buffer

// fs/raw is a stream, not a request: a 40 GB download or a video scrubbed by a
// player under backpressure legitimately lives for hours, and API.md gives it
// no daemon-side deadline. So the relay never counts wall clock against it -
// what it counts is *silence from the daemon*. $FB_TIMEOUT is only the
// granularity of that wait (one fread() blocks that long); the stream is given
// up only after $FB_RAW_STALL seconds with no body byte at all, which covers a
// spun-down disk or a slow 7zz extraction without pinning an fpm worker on a
// wedged daemon forever.
$FB_RAW_STALL    = 300;    // seconds of daemon silence that ends an fs/raw body
// An fs/raw body at or above this size (or of unknown size) is streamed with
// nginx buffering switched off - see "streaming preamble" below. Smaller bodies
// (thumbnails, a player's opening probe range, a small PDF) keep the default
// buffering: nginx takes the whole thing at once and the php-fpm worker is free
// again immediately, which matters because a pool has only a handful of them.
$FB_RAW_NOBUFFER = 8388608;

// A bounded response (anything that is not fs/raw and not an event stream) is
// read into memory and checked against its own framing before any of it is
// committed - see "commit discipline" above. These are API answers: a few
// hundred bytes for an envelope, a few hundred KB for the largest fs/view.
// Beyond this cap we go back to streaming, which cannot verify a length but
// also cannot mislead: a body that stops early against an honest
// Content-Length is a transfer error every HTTP client already detects.
$FB_RELAY_BUFFER = 8388608;

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
// output discipline
// ---------------------------------------------------------------------------
/**
 * Throw away anything already sitting in an output buffer. The only body this
 * script may ever emit is its own envelope or the daemon's bytes; a newline
 * from an auto_prepend or a PHP notice is neither, and left in place it both
 * corrupts the JSON and desyncs the Content-Length we are about to declare.
 * Buffered output has not reached the client yet, so dropping it is safe.
 */
function fb_discard_pending_output() {
  while (ob_get_level() > 0 && ob_get_length() > 0) {
    if (!@ob_clean()) return;   // a non-cleanable handler; nothing we can do
  }
}

// ---------------------------------------------------------------------------
// error envelope - matches the daemon's own {ok:false,error:{code,message}}
// ---------------------------------------------------------------------------
function fb_fail($status, $code, $message) {
  fb_discard_pending_output();
  if (!headers_sent()) {
    http_response_code($status);
    header('Content-Type: application/json');
    header('Cache-Control: no-store');
  }
  // If headers really are gone the status is a lie we cannot retract, but the
  // body is still an envelope, and the SPA reads `ok` before it reads a status.
  echo json_encode(['ok' => false, 'error' => ['code' => $code, 'message' => $message]]);
  exit;
}

/**
 * The single point at which this script commits to a response: status line,
 * headers, and (for a bounded response) the whole body. It refuses to run once
 * output has begun, because from that moment http_response_code() and header()
 * are silent no-ops and every label we would attach is fiction.
 *
 * $headers is an ordered name => value map. $body is the complete body, or null
 * when the caller is about to stream one itself.
 */
function fb_commit($status, array $headers, $body = null) {
  if (headers_sent($file, $line)) {
    fb_fail(502, 'INTERNAL',
      'response already started before the relay could label it (' . $file . ':' . $line . ')');
  }
  fb_discard_pending_output();
  http_response_code($status);
  foreach ($headers as $name => $value) {
    header($name . ': ' . $value);
  }
  if ($body !== null) echo $body;
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

/**
 * Parse an application/x-www-form-urlencoded body ourselves, from the raw
 * bytes, into name => value. Deliberately not parse_str(): that mangles field
 * names (dots and spaces become underscores), silently builds arrays out of
 * `a[]`, and drops everything past max_input_vars. We want the two names we
 * know about and a hard error on anything else.
 *
 * Returns null when the body is not a well-formed pair list.
 */
function fb_parse_form($raw) {
  $out = [];
  if ($raw === '') return $out;
  foreach (explode('&', $raw) as $pair) {
    if ($pair === '') continue;
    $eq = strpos($pair, '=');
    if ($eq === false) return null;                 // a bare flag is not our shape
    $name = urldecode(substr($pair, 0, $eq));
    if ($name === '' || isset($out[$name])) return null;   // empty or duplicated
    $out[$name] = urldecode(substr($pair, $eq + 1));
  }
  return $out;
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

// ---- POST body + CSRF, before anything else -------------------------------
// A form-encoded body means a cross-origin <form> can reach this parser, so
// the token check is the control that keeps it out and it runs first: no other
// parameter is examined, and no socket is opened, until hash_equals() passes.
$body  = '';        // the bytes we will forward to the daemon
$ctype = null;      // the Content-Type we will forward with them
if ($method === 'POST') {
  // Shape. json = the body IS the upstream body (dev server, curl).
  // form = the Unraid envelope: csrf_token + payload (see CSRF above).
  // Multipart is refused outright, which is also what guarantees php://input
  // still holds the raw bytes below.
  $inCtype = fb_clean_header($_SERVER['CONTENT_TYPE'] ?? '');
  $inMime  = ($inCtype === null) ? '' : fb_mime_only($inCtype);
  if ($inMime !== 'application/json' && $inMime !== 'application/x-www-form-urlencoded') {
    fb_fail(415, 'BAD_REQUEST',
      'POST body must be application/json or the application/x-www-form-urlencoded envelope');
  }

  // Raw bytes, never $_POST: both prepend generations unset what they consumed
  // ($_POST['csrf_token'] on 7.2.0, plus $_SERVER['HTTP_X_CSRF_TOKEN'] on
  // master), and php://input is the one copy neither can take away.
  $raw = (string)@file_get_contents('php://input');

  $provided = null;
  if ($inMime === 'application/x-www-form-urlencoded') {
    $fields = fb_parse_form($raw);
    if ($fields === null) {
      fb_fail(400, 'BAD_REQUEST', 'malformed form-encoded body');
    }
    foreach ($fields as $name => $_unused) {
      if ($name !== 'csrf_token' && $name !== 'payload') {
        fb_fail(400, 'BAD_REQUEST', 'unexpected field in the request envelope');
      }
    }
    if (isset($fields['csrf_token'])) {
      $provided = fb_clean_header($fields['csrf_token']);
    }
    $body  = $fields['payload'] ?? '';
    $ctype = 'application/json';
  } else {
    $body  = $raw;
    $ctype = $inCtype;
  }

  // Fallbacks for the json shape and for anything hand-rolled: the header the
  // SPA still sends, then the field PHP parsed for us. Either may already have
  // been consumed by the prepend, which is why the raw body wins above.
  if ($provided === null) {
    $provided = fb_clean_header($_SERVER['HTTP_X_CSRF_TOKEN'] ?? '');
  }
  if ($provided === null && isset($_POST['csrf_token']) && is_string($_POST['csrf_token'])) {
    $provided = fb_clean_header($_POST['csrf_token']);
  }

  $expected = fb_expected_csrf($FB_VAR_INI);
  if ($expected === null) {
    fb_fail(403, 'FORBIDDEN', 'csrf token unavailable');
  }
  if ($provided === null || !hash_equals($expected, $provided)) {
    fb_fail(403, 'FORBIDDEN', 'invalid or missing csrf token');
  }

  // The daemon is handed JSON and nothing else, whichever shape carried it.
  if ($inMime === 'application/x-www-form-urlencoded') {
    if ($body === '') {
      fb_fail(400, 'BAD_REQUEST', 'request envelope has no payload');
    }
    $head = ltrim($body);
    if ($head === '' || ($head[0] !== '{' && $head[0] !== '[')) {
      fb_fail(400, 'BAD_REQUEST', 'payload must be a JSON object or array');
    }
    json_decode($body);
    if (json_last_error() !== JSON_ERROR_NONE) {
      fb_fail(400, 'BAD_REQUEST', 'payload is not valid JSON: ' . json_last_error_msg());
    }
  }
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
if ($method === 'POST') {
  $override = fb_clean_header($_SERVER['HTTP_X_HTTP_METHOD_OVERRIDE'] ?? '');
  if ($override !== null) {
    if (strtoupper($override) !== 'PUT') {
      fb_fail(400, 'BAD_REQUEST', 'unsupported method override');
    }
    $upstreamMethod = 'PUT';
  }
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
// A 1xx is a prelude, not an answer: relaying it as the final status would put
// the real response - status line and all - into the client's body. Skip past
// any that arrive and keep reading. The bound stops a daemon that only ever
// sends preludes from spinning here.
$status   = 0;
$upstream = [];
for ($prelude = 0; $prelude < 8; $prelude++) {
  $statusLine = @fgets($sock, 8192);
  if ($statusLine === false || $statusLine === '') {
    fclose($sock);
    fb_fail(502, 'INTERNAL', 'daemon not running');
  }
  if (!preg_match('#^HTTP/1\.[01]\s+(\d{3})#', $statusLine, $m)) {
    fclose($sock);
    fb_fail(502, 'INTERNAL', 'malformed response from daemon');
  }
  $status   = (int)$m[1];
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
  if ($status < 100 || $status >= 200) break;
}
if ($status < 200) {
  fclose($sock);
  fb_fail(502, 'INTERNAL', 'daemon sent only informational responses');
}

// We never send Accept-Encoding, so a coded body is not something we asked for
// and not something we relay a Content-Encoding for. Passing the coded bytes on
// under a plain Content-Type would be exactly the misrepresentation this bridge
// promises never to make.
$upEnc = strtolower(trim($upstream['content-encoding'] ?? ''));
if ($upEnc !== '' && $upEnc !== 'identity') {
  fclose($sock);
  fb_fail(502, 'INTERNAL', 'daemon sent an encoded body (' . $upEnc . ') that the bridge cannot relay');
}

$upType   = $upstream['content-type'] ?? '';
$isSse    = (stripos($upType, 'text/event-stream') === 0);
$chunked  = (stripos($upstream['transfer-encoding'] ?? '', 'chunked') !== false);
$hasLen   = array_key_exists('content-length', $upstream) && !$chunked;
$bodyLen  = $hasLen ? (int)$upstream['content-length'] : -1;

// ---------------------------------------------------------------------------
// build the outgoing status + whitelisted headers  (nothing is sent yet)
// ---------------------------------------------------------------------------
// These are staged rather than emitted so that fb_commit() stays the one place
// the response is decided - see "commit discipline" at the top. Until then this
// script has produced no output at all and can still answer with an envelope.
$outHeaders = [];

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
    $outHeaders[$name] = $upstream[$key];
  }
}

// ---- raw hardening --------------------------------------------------------
// Belt and braces with the daemon, which enforces the same rules: a plugin and
// a daemon of different vintages must never combine into "arbitrary file body
// rendered as a document on the root-authenticated webGUI origin". These
// entries replace whatever was relayed above.
$isRaw = ($pathOnly === '/api/v1/fs/raw');
if ($isRaw) {
  $outHeaders['X-Content-Type-Options']  = 'nosniff';
  $outHeaders['Content-Security-Policy'] = "default-src 'none'; sandbox";

  $mime   = fb_mime_only($upType);
  $inline = in_array($mime, $FB_RAW_INLINE_TYPES, true);
  if (!$inline) {
    foreach ($FB_RAW_INLINE_PREFIXES as $prefix) {
      if (strncmp($mime, $prefix, strlen($prefix)) === 0) { $inline = true; break; }
    }
  }
  if (!$inline) {
    $outHeaders['Content-Type']        = 'application/octet-stream';
    $outHeaders['Content-Disposition'] = fb_attachment_disposition($upstream['content-disposition'] ?? '');
  }
}

// Content-Length only when we relay the body verbatim. Chunked bodies are
// decoded here and emitted plain, so the upstream length would be wrong; the
// buffered path below replaces it with the length it actually measured.
if ($hasLen && !$isSse) {
  $outHeaders['Content-Length'] = (string)$bodyLen;
}

// ---------------------------------------------------------------------------
// streaming preamble for fs/raw
// ---------------------------------------------------------------------------
// A media player reads a video at whatever rate it needs, so an fs/raw response
// spends most of its life blocked in a write to a client that is not asking for
// more yet. Everything that would put a clock or a buffer between that client
// and this loop has to go, exactly like the SSE branch below does it:
//
//   - set_time_limit(0): the relay is a stream. max_execution_time is finite in
//     php.ini, and on a PHP built with zend_max_execution_timers it counts wall
//     clock - including time blocked writing to a slow player - so a paused or
//     throttled video would die on the timer with a truncated body.
//   - zlib.output_compression off: we relay the daemon's Content-Length (and
//     Content-Range) verbatim, so nothing may re-encode the body underneath it.
//   - no userland output buffer: slabs go straight to the SAPI, which keeps
//     memory flat on a multi-GB download and makes the client's backpressure -
//     and its disappearance - visible to this loop.
//   - X-Accel-Buffering: no (big/unknown bodies only): without it nginx happily
//     buffers the response into its temp dir (RAM on Unraid) and answers the
//     player itself, which both defeats the backpressure above and can spool
//     gigabytes nobody asked for. Below $FB_RAW_NOBUFFER we leave buffering on
//     so a thumbnail does not hold an fpm worker for the client's whole read.
//   - ignore_user_abort stays FALSE (unlike SSE, which owns its own lifetime):
//     when the player closes the tab or seeks elsewhere, this request must end
//     promptly so the daemon stops reading the file. $emit's connection_aborted()
//     check is the graceful path; PHP aborting the request outright is fine too.
if ($isRaw) {
  if (!$hasLen || $bodyLen >= $FB_RAW_NOBUFFER) {
    $outHeaders['X-Accel-Buffering'] = 'no';
  }
  fb_commit($status, $outHeaders);          // headers staged; still no output
  @set_time_limit(0);
  @ini_set('zlib.output_compression', 'Off');
  @ini_set('implicit_flush', '1');
  while (ob_get_level() > 0) {
    if (!@ob_end_flush()) break;
  }
  ob_implicit_flush(true);
  ignore_user_abort(false);
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
  // nginx sits in front of php-fpm; without X-Accel-Buffering it would buffer
  // the stream.
  $outHeaders['X-Accel-Buffering'] = 'no';
  $outHeaders['Cache-Control']     = 'no-cache';
  $outHeaders['Connection']        = 'keep-alive';
  fb_commit($status, $outHeaders);
  @ini_set('zlib.output_compression', 'Off');
  @ini_set('output_buffering', 'Off');
  @ini_set('implicit_flush', '1');
  while (ob_get_level() > 0) {
    if (!@ob_end_flush()) break;
  }
  ob_implicit_flush(true);
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

/**
 * An empty read means one of three things and they are not the same: end of
 * body (feof), a read that merely hit the socket's inactivity timeout, or an
 * error. Treating the middle case as "done" is what silently truncates a stream
 * whose producer went quiet for a while - a disk spinning up, a 7zz extraction
 * inside an archive, a daemon busy elsewhere - leaving the client a short body
 * against an honest Content-Length and no way to tell it was cut.
 *
 * So on an fs/raw body we go back and wait again, for as long as the client is
 * still there and the daemon has not been silent for $FB_RAW_STALL ($giveUp is
 * the caller's per-stall state; a byte from the daemon resets it). Non-raw
 * responses keep the old behaviour: the daemon holds a 30 s deadline on those,
 * so silence past $FB_TIMEOUT means it is wedged, not slow.
 */
$keep_waiting = function (&$giveUp) use ($sock, $timed_out, $isRaw, $FB_RAW_STALL) {
  if (feof($sock)) return false;                    // end of body
  if (!$timed_out($sock)) return false;             // a real read error
  if (!$isRaw) return false;                        // bounded response: wedged
  if (connection_aborted()) return false;           // nobody left to stream to
  if ($giveUp === null) $giveUp = time() + $FB_RAW_STALL;
  return time() < $giveUp;                          // else: daemon gone quiet
};

/** Body bytes from the daemon; '' only when the body is really over. */
$read_body = function ($max) use ($sock, $keep_waiting) {
  $giveUp = null;
  while (true) {
    $buf = @fread($sock, $max);
    if ($buf !== false && $buf !== '') return $buf;
    if (!$keep_waiting($giveUp)) return '';
  }
};

/** One framing line (chunk sizes); false only when the body is really over. */
$read_line = function ($max) use ($sock, $keep_waiting) {
  $giveUp = null;
  while (true) {
    $line = @fgets($sock, $max);
    if ($line !== false && $line !== '') return $line;
    if (!$keep_waiting($giveUp)) return false;
  }
};

// ---------------------------------------------------------------------------
// faithful-or-fail
// ---------------------------------------------------------------------------
// fs/raw has already committed above and streams; it is a file transfer, where
// a stall is normal and buffering is not an option. Everything else is an API
// answer of bounded size, so we read it in full and check it against its own
// framing FIRST, and only then commit. A daemon that stops mid-body therefore
// produces a 502 envelope, not a 200 with a plausible-looking short body.
//
// $sink is what makes that safe for a body we cannot size in advance (chunked,
// or close-delimited): it accumulates until $FB_RELAY_BUFFER, and if the body
// turns out to be bigger than that it commits what it has - without a
// Content-Length, because on those framings we never promised one - and
// switches to streaming the rest. Nothing is buffered without bound and nothing
// is promised that we might not deliver.
$streaming = $isRaw;
$bodyBuf   = '';

$commit_stream = function ($dropLength = false) use (&$streaming, &$outHeaders, $status) {
  $h = $outHeaders;
  if ($dropLength) unset($h['Content-Length']);
  fb_commit($status, $h);
  $streaming = true;
};

$sink = function ($data) use (&$bodyBuf, &$streaming, &$commit_stream, $emit, $FB_RELAY_BUFFER) {
  if ($streaming) return $emit($data);
  $bodyBuf .= $data;
  if (strlen($bodyBuf) <= $FB_RELAY_BUFFER) return true;
  $commit_stream(true);
  $out     = $bodyBuf;
  $bodyBuf = '';
  return $emit($out);
};

// A Content-Length framed body too big to hold is streamed against the
// daemon's own honest length - there is nothing for us to verify that the
// client cannot verify itself.
if (!$streaming && $hasLen && $bodyLen > $FB_RELAY_BUFFER) {
  $commit_stream();
}

$truncated = false;

if ($status === 204 || $status === 304) {
  // no body

} elseif ($chunked) {
  // ---- chunked transfer coding: decode, emit plain ------------------------
  $stop     = false;
  $sawFinal = false;
  while (!$stop && !feof($sock)) {
    $line = $read_line(1024);
    if ($line === false) break;
    $line = trim($line);
    if ($line === '') continue;                       // CRLF between chunks
    $semi = strpos($line, ';');                       // chunk extensions
    if ($semi !== false) $line = substr($line, 0, $semi);
    $line = trim($line);
    if ($line === '' || !preg_match('/^[0-9A-Fa-f]{1,8}$/D', $line)) break;
    $remaining = hexdec($line);
    if ($remaining === 0) { $sawFinal = true; break; } // last chunk (trailers ignored)
    while ($remaining > 0) {
      $buf = $read_body(min($FB_CHUNK, $remaining));
      if ($buf === '') {                              // end of body
        $stop = true;
        break;
      }
      $remaining -= strlen($buf);
      if (!$sink($buf)) {                             // client went away
        $stop = true;
        break;
      }
    }
    if ($stop) break;
    @fread($sock, 2);                                 // trailing CRLF
  }
  // No terminating 0-chunk means the daemon died mid-answer.
  if (!$sawFinal && !$stop) $truncated = true;

} elseif ($hasLen) {
  // ---- Content-Length framing --------------------------------------------
  $remaining = $bodyLen;
  while ($remaining > 0 && !feof($sock)) {
    $buf = $read_body(min($FB_CHUNK, $remaining));
    if ($buf === '') break;
    $remaining -= strlen($buf);
    if (!$sink($buf)) break;
  }
  if ($remaining > 0 && !$streaming) $truncated = true;

} else {
  // ---- read until the daemon closes (we asked for Connection: close) ------
  while (!feof($sock)) {
    $buf = $read_body($FB_CHUNK);
    if ($buf === '') break;
    if (!$sink($buf)) break;
  }
}

fclose($sock);

if (!$streaming) {
  if ($truncated) {
    fb_fail(502, 'INTERNAL', 'truncated response from daemon: ' . strlen($bodyBuf)
      . ' of ' . ($hasLen ? $bodyLen . ' bytes' : 'an unfinished chunked body'));
  }
  // The measured length is authoritative: it equals the upstream one on a
  // Content-Length body (we just verified that) and is the only correct value
  // for a body we de-chunked.
  if ($status !== 204 && $status !== 304) {
    $outHeaders['Content-Length'] = (string)strlen($bodyBuf);
  }
  fb_commit($status, $outHeaders, $bodyBuf);
}
