<?php
/* include/settings.php - shared helpers for the File Browser webGUI pages
 *
 * Copyright 2026, James Smyth
 *
 * Licensed under the MIT License; see the LICENSE file in the project root.
 *
 * Nothing in this file executes a shell command, and no value read from
 * settings.cfg is ever passed to one: the config file is written by the
 * webGUI's /update.php from raw POST field names/values, so it is data, never
 * script. rc.filebrowserd and the event hooks read it with a literal sed
 * extraction for the same reason - nothing sources it. The only place the
 * plugin shells out at all is include/exec.php, and there only to a fixed path
 * with a whitelisted verb that never includes a config value.
 */

if (!defined('FB_PLUGIN')) {
  define('FB_PLUGIN',  'filebrowser');
  define('FB_PLUGDIR', '/usr/local/emhttp/plugins/filebrowser');
  define('FB_BOOTDIR', '/boot/config/plugins/filebrowser');
  define('FB_CFG',     '/boot/config/plugins/filebrowser/settings.cfg');
  define('FB_DEFAULTS','/usr/local/emhttp/plugins/filebrowser/default.cfg');
  define('FB_SOCKET',  '/var/run/filebrowserd.sock');
  define('FB_PIDFILE', '/var/run/filebrowserd.pid');
  define('FB_LOGFILE', '/var/log/filebrowserd.log');
  define('FB_RC',      '/usr/local/emhttp/plugins/filebrowser/rc.filebrowserd');
  define('FB_DEFAULT_DATA_DIR',     '/mnt/user/appdata/filebrowser');
  define('FB_DEFAULT_BROWSE_ROOTS', '/mnt/user');
  // Must stay identical to FB_ABS_RE / FB_ROOT_RE in rc.filebrowserd.
  define('FB_ABS_RE',  '#^/[A-Za-z0-9._@/ -]+$#D');
  define('FB_ROOT_RE', '#^/[A-Za-z0-9._@/ -]*$#D');
}

/** True for an acceptable DATA_DIR: one absolute path, not the filesystem root. */
function fb_valid_data_dir($value) {
  if (!is_string($value) || $value === '' || $value === '/') return false;
  return (bool)preg_match(FB_ABS_RE, $value);
}

/** True for an acceptable BROWSE_ROOTS: comma-separated absolute paths. */
function fb_valid_browse_roots($value) {
  if (!is_string($value) || $value === '') return false;
  if (substr($value, -1) === ',') return false;
  foreach (explode(',', $value) as $item) {
    if ($item === '' || !preg_match(FB_ROOT_RE, $item)) return false;
  }
  return true;
}

/**
 * Plugin defaults overlaid with the user's copy on the flash drive, then
 * validated. Same precedence and the same rules rc.filebrowserd applies when
 * it reads the two files, so the page never shows a value the daemon would
 * refuse to start with.
 */
function fb_settings() {
  $cfg = [
    'SERVICE'      => 'enable',
    'DATA_DIR'     => FB_DEFAULT_DATA_DIR,
    'BROWSE_ROOTS' => FB_DEFAULT_BROWSE_ROOTS,
  ];
  foreach ([FB_DEFAULTS, FB_CFG] as $file) {
    if (is_readable($file)) {
      $parsed = @parse_ini_file($file);
      if (is_array($parsed)) $cfg = array_merge($cfg, $parsed);
    }
  }
  // Anything unexpected is replaced by the shipped default and flagged, so the
  // settings page can say so instead of silently disagreeing with the daemon.
  $cfg['INVALID'] = [];
  if ($cfg['SERVICE'] !== 'enable' && $cfg['SERVICE'] !== 'disable') {
    $cfg['INVALID'][] = 'SERVICE';
    $cfg['SERVICE'] = 'enable';
  }
  if (!fb_valid_data_dir($cfg['DATA_DIR'] ?? null)) {
    $cfg['INVALID'][] = 'DATA_DIR';
    $cfg['DATA_DIR'] = FB_DEFAULT_DATA_DIR;
  }
  if (!fb_valid_browse_roots($cfg['BROWSE_ROOTS'] ?? null)) {
    $cfg['INVALID'][] = 'BROWSE_ROOTS';
    $cfg['BROWSE_ROOTS'] = FB_DEFAULT_BROWSE_ROOTS;
  }
  return $cfg;
}

/** PID of the running daemon, or 0. Mirrors rc.filebrowserd's running(). */
function fb_pid() {
  if (!is_file(FB_PIDFILE)) return 0;
  $pid = trim((string)@file_get_contents(FB_PIDFILE));
  if ($pid === '' || !ctype_digit($pid)) return 0;
  return is_dir('/proc/' . $pid) ? (int)$pid : 0;
}

/** The socket is what the proxy actually needs; check it separately. */
function fb_socket_present() {
  return file_exists(FB_SOCKET);
}

/** Normalised webGUI theme name for the SPA: black|white|azure|gray. */
function fb_theme($display) {
  // $display['theme'] can carry a variant suffix (e.g. "black-..."); the webGUI
  // itself narrows it the same way in ThemeHelper::__construct().
  $theme = strtok((string)($display['theme'] ?? ''), '-');
  return in_array($theme, ['black', 'white', 'azure', 'gray'], true) ? $theme : 'white';
}
