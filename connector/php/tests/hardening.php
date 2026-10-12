<?php
/**
 * WordPress hardening (connector/SPEC.md 2.12) against the stand-in in wp.php.
 *
 *   php connector/php/tests/hardening.php
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

use CloudGround\Connector\Hardening;

require __DIR__ . '/wp.php';
require __DIR__ . '/check.php';

// What the panel puts in the PHP master's pool.
putenv('CLOUDGROUND_HARDENING=xmlrpc,file_edit,version,bogus');
putenv('CLOUDGROUND_LOGIN_PATH=my-door');
require dirname(__DIR__) . '/cloudground-connector.php';

$removed = Site::$removed;
Site::reset();
Site::$removed = $removed;

section('items');
check('the known items, from the environment', Hardening::items(), ['xmlrpc', 'file_edit', 'version']);
check('the login path', Hardening::loginPath(), 'my-door');
putenv('CLOUDGROUND_LOGIN_PATH=../x');
check('a malformed login path is ignored', Hardening::loginPath(), '');
putenv('CLOUDGROUND_LOGIN_PATH=my-door');
check('the hooks are in place', [isset(Site::$hooks['plugins_loaded'][PHP_INT_MIN]), isset(Site::$hooks['map_meta_cap'][10]), isset(Site::$hooks['site_url'][PHP_INT_MAX]), isset(Site::$hooks['init'][0])], [true, true, true, true]);
check('the generator, RSD link and admin-location redirects are removed', array_values(array_intersect(Site::$removed, ['wp_head:wp_generator', 'wp_head:rsd_link', 'template_redirect:wp_redirect_admin_locations'])), ['wp_head:rsd_link', 'wp_head:wp_generator', 'template_redirect:wp_redirect_admin_locations']);

section('XML-RPC');
check('nobody by default', Hardening::xmlrpcAllowed(null, null, false), false);
check('an address on the exception list (nginx says 1)', Hardening::xmlrpcAllowed(null, '1', false), true);
check('nginx says 0', Hardening::xmlrpcAllowed(null, '0', false), false);
check('Jetpack active, its own request', Hardening::xmlrpcAllowed('jetpack', null, true), true);
check('for=jetpack without Jetpack: no', Hardening::xmlrpcAllowed('jetpack', null, false), false);
check('Jetpack active, another request: no', Hardening::xmlrpcAllowed(['jetpack'], null, true), false);
check('the X-Pingback header goes', Hardening::noPingbackHeader(['X-Pingback' => 'https://example.com/xmlrpc.php', 'A' => 'b']), ['A' => 'b']);

section('file editor');
check('theme and plugin editors are not allowed', [Hardening::noFileEdit(['manage_options'], 'edit_themes'), Hardening::noFileEdit(['edit_plugins'], 'edit_plugins')], [['manage_options', 'do_not_allow'], ['edit_plugins', 'do_not_allow']]);
check('other capabilities untouched', Hardening::noFileEdit(['edit_posts'], 'edit_posts'), ['edit_posts']);

section('version');
$tag = substr(hash_hmac('sha256', '7.1.3', wp_salt('nonce')), 0, 8);
check('a core asset loses the version', Hardening::assetVersion('https://example.com/wp-includes/js/jquery.js?ver=7.1.3'), 'https://example.com/wp-includes/js/jquery.js?ver=' . $tag);
check('… which still changes with it', $tag !== substr(hash_hmac('sha256', '7.1.4', wp_salt('nonce')), 0, 8), true);
check('a plugin version is not the core one', Hardening::assetVersion('https://example.com/a.css?ver=7.1.30'), 'https://example.com/a.css?ver=7.1.30');
check('in the middle of the query', Hardening::assetVersion('/a.js?ver=7.1.3&x=1'), '/a.js?ver=' . $tag . '&x=1');
check('the generator is empty', apply_filters('the_generator', '<meta name="generator" content="WordPress 7.1.3">'), '');

section('login path');
check('site_url', apply_filters('site_url', 'https://example.com/wp-login.php?action=lostpassword'), 'https://example.com/my-door?action=lostpassword');
check('a bare one', Hardening::loginURL('https://example.com/wp-login.php'), 'https://example.com/my-door');
check('a longer name is left alone', Hardening::loginURL('https://example.com/wp-login.php.bak'), 'https://example.com/wp-login.php.bak');
check("core's relative redirect after logout", Hardening::loginRedirect('wp-login.php?loggedout=true'), 'my-door?loggedout=true');
check('an absolute redirect on this site', Hardening::loginRedirect('https://example.com/wp-login.php?checkemail=confirm'), 'https://example.com/my-door?checkemail=confirm');
check('another site is left alone', Hardening::loginRedirect('https://other.example/wp-login.php'), 'https://other.example/wp-login.php');
check('wp-admin for a visitor: hidden', Hardening::adminHidden(true, 'index.php', false, false), true);
check('admin-ajax.php stays open', Hardening::adminHidden(true, 'admin-ajax.php', false, false), false);
check('admin-post.php stays open', Hardening::adminHidden(true, 'admin-post.php', false, false), false);
check('Ajax stays open', Hardening::adminHidden(true, 'index.php', true, false), false);
check('signed in: shown', Hardening::adminHidden(true, 'index.php', false, true), false);
check('the front end: untouched', Hardening::adminHidden(false, 'index.php', false, false), false);

done();
