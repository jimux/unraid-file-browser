<?php
/* include/exec.php - Start / Stop / Restart handler for the File Browser pages
 *
 * Copyright 2026, James Smyth
 *
 * Licensed under the MIT License; see the LICENSE file in the project root.
 *
 * ---------------------------------------------------------------------------
 * This is the ONLY file in the plugin that runs a subprocess.
 *
 *   - POST only. Unraid's auto_prepend_file (local_prepend.php) has already
 *     verified the webGUI csrf_token by the time this script starts, so a
 *     cross-site POST cannot reach the code below.
 *   - `action` is compared against a fixed whitelist and the *whitelist entry*
 *     is what gets concatenated into the command - the request string itself
 *     never reaches the shell, so there is nothing to escape and nothing to
 *     inject.
 *   - The command is a fixed absolute path. No user input contributes any
 *     other part of it.
 * ---------------------------------------------------------------------------
 */

require_once __DIR__ . '/settings.php';

header('Content-Type: application/json');
header('Cache-Control: no-store');

function fb_exec_fail($status, $message) {
  http_response_code($status);
  echo json_encode(['ok' => false, 'error' => ['code' => 'BAD_REQUEST', 'message' => $message]]);
  exit;
}

if (($_SERVER['REQUEST_METHOD'] ?? 'GET') !== 'POST') {
  header('Allow: POST');
  fb_exec_fail(405, 'POST required');
}

$requested = $_POST['action'] ?? '';
$allowed   = ['start', 'stop', 'restart', 'status'];

$action = null;
foreach ($allowed as $candidate) {
  if ($requested === $candidate) { $action = $candidate; break; }
}
if ($action === null) {
  fb_exec_fail(400, 'unknown action');
}

if (!is_executable(FB_RC)) {
  http_response_code(500);
  echo json_encode(['ok' => false, 'error' => [
    'code'    => 'INTERNAL',
    'message' => 'rc.filebrowserd is missing - reinstall the plugin',
  ]]);
  exit;
}

$output = [];
$rc     = 0;
// Fixed literal command + a verb taken from the whitelist above.
exec('/usr/local/emhttp/plugins/filebrowser/rc.filebrowserd ' . $action . ' 2>&1', $output, $rc);

// Give a start/restart a moment to settle before reporting state back.
if ($action === 'start' || $action === 'restart') {
  for ($i = 0; $i < 20 && !fb_socket_present(); $i++) usleep(100000);
}

echo json_encode(['ok' => true, 'data' => [
  'action'  => $action,
  'rc'      => $rc,
  'running' => fb_pid() > 0,
  'pid'     => fb_pid(),
  'socket'  => fb_socket_present(),
  'output'  => implode("\n", $output),
]]);
