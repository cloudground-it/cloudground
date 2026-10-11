<?php
/**
 * Two-factor sign-in (connector/SPEC.md 2.10) against the stand-in in wp.php.
 *
 *   php connector/php/tests/two-factor.php
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

use CloudGround\Connector\Base32;
use CloudGround\Connector\Reply;
use CloudGround\Connector\Seal;
use CloudGround\Connector\Totp;
use CloudGround\Connector\TwoFactor;

require __DIR__ . '/wp.php';
require __DIR__ . '/check.php';

// What the panel puts in the PHP master's pool (env[CLOUDGROUND_2FA]).
putenv('CLOUDGROUND_2FA=required');
define('CLOUDGROUND_SITE_TOKEN', 'site-token');
require dirname(__DIR__) . '/cloudground-connector.php';

/** The pending sign-in's cookie value from a Reply of begin(). */
function cookieOf(Reply $r): string
{
    return $r->cookies[TwoFactor::COOKIE][0] ?? '';
}

function formOf(string $cookie): string
{
    return hash_hmac('sha256', 'form', explode('.', $cookie)[1]);
}

function codeNow(string $secret, int $offset = 0): string
{
    return Totp::at($secret, intdiv(time(), Totp::PERIOD) + $offset);
}

/** Enrols user 1 with a known key and returns it. */
function enrol(int $user = 1): string
{
    $secret = Totp::secret();
    update_user_meta($user, TwoFactor::SECRET, Seal::close($secret));
    return $secret;
}

section('TOTP (RFC 6238 vectors)');
$rfc = Base32::encode('12345678901234567890');
check('base32 of the RFC key', $rfc, 'GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ');
check('base32 round trip', Base32::decode($rfc), '12345678901234567890');
check('base32 refuses other characters', Base32::decode('GEZ1'), null);
check('T=59', Totp::at($rfc, intdiv(59, 30)), '287082');
check('T=1111111109', Totp::at($rfc, intdiv(1111111109, 30)), '081804');
check('T=1234567890', Totp::at($rfc, intdiv(1234567890, 30)), '005924');
check('T=2000000000', Totp::at($rfc, intdiv(2000000000, 30)), '279037');
$t = 1234567890;
check('the current code', Totp::verify($rfc, '005924', 0, $t), intdiv($t, 30));
check('one step of drift is accepted', Totp::verify($rfc, Totp::at($rfc, intdiv($t, 30) - 1), 0, $t), intdiv($t, 30) - 1);
check('two steps are not', Totp::verify($rfc, Totp::at($rfc, intdiv($t, 30) - 2), 0, $t), 0);
check('a code already used is refused', Totp::verify($rfc, '005924', intdiv($t, 30), $t), 0);
check('not six digits', Totp::verify($rfc, '05924', 0, $t), 0);
check('a new key is 160 bits', strlen((string) Base32::decode(Totp::secret())), 20);

section('sealed secrets');
Site::reset();
$sealed = Seal::close('JBSWY3DPEHPK3PXP');
check('sealed, not stored in clear', str_contains($sealed, 'JBSWY3DPEHPK3PXP'), false);
check('opens with the same salts', Seal::open($sealed), 'JBSWY3DPEHPK3PXP');
check('a fresh nonce each time', Seal::close('x') !== Seal::close('x'), true);
check('tampered: nothing', Seal::open(substr($sealed, 0, -2) . 'AA'), null);
Site::$options['salt_suffix'] = '-rotated';
check('new salts: nothing (fails closed)', Seal::open($sealed), null);
Site::reset();
check('not a sealed value', Seal::open(['x']), null);

section('which sign-ins are held');
Site::reset();
$_SERVER['REQUEST_METHOD'] = 'POST';
$admin = get_userdata(1);
$plain = get_userdata(2);
check('mode from the environment', TwoFactor::mode(), 'required');
check('the hooks are in place', isset(Site::$hooks['authenticate'][99999], Site::$hooks['login_form_cloudground_2fa'][10]), true);
check('a failed password is left alone', TwoFactor::intercept(new WP_Error('incorrect_password'), 'user1', 'x', [], true) instanceof WP_Error, true);
check('a user without the role and without an app signs in', TwoFactor::intercept($plain, 'user2', 'pw', [], true), $plain);
check('the auth cookie (no credentials) is not asked again', TwoFactor::intercept($admin, '', '', [], true), $admin);
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], false);
check('XML-RPC and the REST API: refused, not signed in', $r instanceof WP_Error && isset($r->errors['cloudground_2fa']), true);
Site::$actions['application_password_did_authenticate'] = 1;
check('an application password is its own credential', TwoFactor::intercept($admin, 'user1', 'app-pw', [], false), $admin);
unset(Site::$actions['application_password_did_authenticate']);
putenv('CLOUDGROUND_2FA=optional');
check('optional: an administrator without an app signs in', TwoFactor::intercept($admin, 'user1', 'pw', [], true), $admin);
enrol(2);
check('optional: anyone with an app is held', TwoFactor::intercept($plain, 'user2', 'pw', [], true) instanceof Reply, true);
putenv('CLOUDGROUND_2FA=off');
check('off: nobody is held, even with an app', TwoFactor::intercept($plain, 'user2', 'pw', [], true), $plain);
putenv('CLOUDGROUND_2FA=required');
$shop = new WP_User(3, ['edit_posts' => true, 'read' => true], ['shop_manager']);
Site::$users[3] = $shop;
check('required: a shop manager without an app is held', TwoFactor::intercept($shop, 'user3', 'pw', [], true) instanceof Reply, true);

section('second step, enrolled account');
Site::reset();
$secret = enrol(1);
$r = TwoFactor::intercept($admin, 'user1', 'pw', ['rememberme' => 'forever', 'redirect_to' => 'https://example.com/wp-admin/edit.php'], true);
check('a correct password is held: no session', [$r->status, $r->user], [303, 0]);
check('sent to the second step', $r->headers['Location'], 'https://example.com/wp-login.php?action=cloudground_2fa');
$cookie = cookieOf($r);
check('the cookie carries the user and a 256-bit token', preg_match('/\A1\.[0-9a-f]{64}\z/', $cookie), 1);
$pending = get_user_meta(1, TwoFactor::PENDING, true);
check('only the token hash is stored', [str_contains(serialize($pending), explode('.', $cookie)[1]), $pending['hash']], [false, hash('sha256', explode('.', $cookie)[1])]);
check('no email to an enrolled account', Site::$mails, []);
$page = TwoFactor::step('GET', $cookie, []);
check('the page asks for the code', [$page->status, str_contains($page->body, 'name="cg2fa_code"'), str_contains($page->body, formOf($cookie))], [200, true, true]);
check('a forged cookie gets nothing', str_contains(TwoFactor::step('GET', '1.' . str_repeat('a', 64), [])->body, 'expired'), true);
check('another user id with the token gets nothing', str_contains(TwoFactor::step('GET', '2.' . explode('.', $cookie)[1], [])->body, 'expired'), true);
$r = TwoFactor::step('POST', $cookie, ['cg2fa_code' => codeNow($secret)]);
check('without the form token: no sign-in', $r->user, 0);
$r = TwoFactor::step('POST', $cookie, ['cg2fa_code' => '000000', 'cg2fa_form' => formOf($cookie)]);
check('a wrong code: no sign-in, said so', [$r->user, str_contains($r->body, 'not right')], [0, true]);
$r = TwoFactor::step('POST', $cookie, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($cookie)]);
check('the right code signs in, remembered', [$r->status, $r->user, $r->remember], [303, 1, true]);
check('to where the sign-in asked', $r->headers['Location'], 'https://example.com/wp-admin/edit.php');
check('the cookie is cleared', $r->cookies[TwoFactor::COOKIE][0], '');
check('the pending sign-in is gone', get_user_meta(1, TwoFactor::PENDING, true), '');
$r->then?->__invoke();
check('wp_login fires, as with a password sign-in', did_action('wp_login'), 1);
check('the same cookie cannot sign in again', TwoFactor::step('POST', $cookie, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($cookie)])->user, 0);
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
$c2 = cookieOf($r);
check('the same app code cannot be used twice', TwoFactor::step('POST', $c2, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($c2)])->user, 0);

section('redirects');
Site::reset();
$secret = enrol(1);
$r = TwoFactor::intercept($admin, 'user1', 'pw', ['redirect_to' => 'https://evil.example/'], true);
$c = cookieOf($r);
$r = TwoFactor::step('POST', $c, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($c)]);
check('another host is never a destination', $r->headers['Location'], 'https://example.com/wp-admin/');
add_filter('login_redirect', fn ($to) => 'https://evil.example/x');
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
$c = cookieOf($r);
check('nor through login_redirect', TwoFactor::step('POST', $c, ['cg2fa_code' => codeNow($secret, 1), 'cg2fa_form' => formOf($c)])->headers['Location'], 'https://example.com/wp-admin/');
Site::reset();
enrol(1);
$r = TwoFactor::intercept($admin, 'user1', 'pw', ['redirect' => 'https://example.com/my-account/', 'interim-login' => '1'], true);
check('the interim (session expired) dialog keeps its flag', $r->headers['Location'], 'https://example.com/wp-login.php?action=cloudground_2fa&interim-login=1');
check("WooCommerce's redirect field is kept", get_user_meta(1, TwoFactor::PENDING, true)['redirect'], 'https://example.com/my-account/');

section('recovery codes');
Site::reset();
enrol(1);
$codes = TwoFactor::newCodes(1);
check('ten codes', count($codes), 10);
check('of 64 bits each', preg_match('/\A[0-9a-f]{4}(-[0-9a-f]{4}){3}\z/', $codes[0]), 1);
check('stored as hashes only', str_contains(serialize(Site::$meta[1]), str_replace('-', '', $codes[0])), false);
check('a recovery code signs in', TwoFactor::check(1, strtoupper($codes[3])), true);
check('once', TwoFactor::check(1, $codes[3]), false);
check('nine left', count(get_user_meta(1, TwoFactor::RECOVERY, true)), 9);
check('spelled without dashes too', TwoFactor::check(1, str_replace('-', ' ', $codes[4])), true);
check('a guess is refused', TwoFactor::check(1, '0123-4567-89ab-cdef'), false);
update_user_meta(1, TwoFactor::SECRET, 'v1:garbage');
check('an unreadable key is a wrong code, never a skipped check', TwoFactor::check(1, '123456'), false);

section('wrong codes');
Site::reset();
$secret = enrol(1);
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
$c = cookieOf($r);
for ($i = 0; $i < TwoFactor::TRIES; $i++) {
    TwoFactor::step('POST', $c, ['cg2fa_code' => '000000', 'cg2fa_form' => formOf($c)]);
}
check('five wrong codes: the account waits a minute', TwoFactor::throttled(1) > 0 && TwoFactor::throttled(1) <= 60, true);
$r = TwoFactor::step('POST', $c, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($c)]);
check('even the right code waits', [$r->user, str_contains($r->body, 'Try again in 1 minutes')], [0, true]);
update_user_meta(1, TwoFactor::FAILS, '5:' . (time() - 61));
$r = TwoFactor::step('POST', $c, ['cg2fa_code' => codeNow($secret), 'cg2fa_form' => formOf($c)]);
check('a sign-in takes five codes at most, then starts again', [$r->user, str_contains($r->body, 'sign in again'), get_user_meta(1, TwoFactor::PENDING, true)], [0, true, '']);
update_user_meta(1, TwoFactor::FAILS, '7:1000');
check('the wait doubles: 240 s after seven', TwoFactor::throttled(1, 1000), 240);
check('and runs out', TwoFactor::throttled(1, 1240), 0);
update_user_meta(1, TwoFactor::FAILS, '40:1000');
check('… and an hour at most', TwoFactor::throttled(1, 1000), 3600);
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
update_user_meta(1, TwoFactor::FAILS, '1:' . time());
$c = cookieOf($r);
TwoFactor::step('POST', $c, ['cg2fa_code' => codeNow($secret, 1), 'cg2fa_form' => formOf($c)]);
check('a sign-in clears the count', get_user_meta(1, TwoFactor::FAILS, true), '');
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
$c = cookieOf($r);
$p = get_user_meta(1, TwoFactor::PENDING, true);
$p['expires'] = time() - 1;
update_user_meta(1, TwoFactor::PENDING, $p);
check('after ten minutes the sign-in has expired', str_contains(TwoFactor::step('GET', $c, [])->body, 'expired'), true);

section('setting up at sign-in (required, no app yet)');
Site::reset();
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
$c = cookieOf($r);
check('held, with no session', [$r->status, $r->user], [303, 0]);
check('a confirmation code by email', count(Site::$mails), 1);
check('to the account', Site::$mails[0][0], 'user1@example.com');
preg_match('/\b(\d{4}) (\d{4})\b/', Site::$mails[0][2], $m);
$mail = $m[1] . $m[2];
$p = get_user_meta(1, TwoFactor::PENDING, true);
check('the code is not stored in clear', str_contains(serialize($p), $mail), false);
$new = Seal::open($p['secret']);
check('a new key, sealed', strlen((string) $new), 32);
$page = TwoFactor::step('GET', $c, []);
check('the page shows the key, its link and both fields', [str_contains($page->body, trim(chunk_split($new, 4, ' '))), str_contains($page->body, 'otpauth://totp/'), str_contains($page->body, 'cg2fa_mail'), str_contains($page->body, 'u****@example.com')], [true, true, true, true]);
check('nothing is enrolled yet', TwoFactor::enrolled(1), false);
$r = TwoFactor::step('POST', $c, ['cg2fa_mail' => '00000000', 'cg2fa_code' => codeNow($new), 'cg2fa_form' => formOf($c)]);
check('the app code without the emailed code: no', [$r->user, TwoFactor::enrolled(1)], [0, false]);
$r = TwoFactor::step('POST', $c, ['cg2fa_mail' => $mail, 'cg2fa_code' => '000000', 'cg2fa_form' => formOf($c)]);
check('the emailed code without the app code: no', [$r->user, TwoFactor::enrolled(1)], [0, false]);
$r = TwoFactor::step('POST', $c, ['cg2fa_mail' => $m[1] . ' ' . $m[2], 'cg2fa_code' => codeNow($new), 'cg2fa_form' => formOf($c)]);
check('both: enrolled and signed in', [$r->status, $r->user, TwoFactor::enrolled(1)], [200, 1, true]);
check('the recovery codes are shown once, on that page', preg_match_all('/[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}/', $r->body), 10);
check('the key is the one shown', Seal::open(get_user_meta(1, TwoFactor::SECRET, true)), $new);
Site::reset();
Site::$mailWorks = false;
$r = TwoFactor::intercept($admin, 'user1', 'pw', [], true);
check('no mail: the page says so', str_contains(TwoFactor::step('GET', cookieOf($r), [])->body, 'could not be sent'), true);

section('profile');
Site::reset();
Site::$current = 1;
ob_start();
TwoFactor::profile($admin);
$html = ob_get_clean();
check('an account without an app is offered one', [str_contains($html, 'cg2fa_setup_code'), str_contains($html, 'Required for this account')], [true, true]);
$setup = Seal::open(get_user_meta(1, TwoFactor::SETUP, true)['secret']);
ob_start();
TwoFactor::profile($admin);
ob_end_clean();
check('a reload keeps the same key', Seal::open(get_user_meta(1, TwoFactor::SETUP, true)['secret']), $setup);
$nonce = wp_create_nonce('cloudground_2fa_profile_1');
$_POST = ['_cg2fa_nonce' => $nonce, 'cg2fa_setup_code' => '000000'];
TwoFactor::save(1);
$errors = new WP_Error();
TwoFactor::errors($errors);
check('a wrong code is reported and enrols nothing', [isset($errors->errors['cloudground_2fa']), TwoFactor::enrolled(1)], [true, false]);
$_POST = ['_cg2fa_nonce' => 'forged', 'cg2fa_setup_code' => codeNow($setup)];
TwoFactor::save(1);
check('without the nonce nothing changes', TwoFactor::enrolled(1), false);
$_POST = ['_cg2fa_nonce' => $nonce, 'cg2fa_setup_code' => codeNow($setup)];
TwoFactor::save(1);
check('the right code enrols', TwoFactor::enrolled(1), true);
ob_start();
TwoFactor::profile($admin);
$html = ob_get_clean();
check('the codes are shown once', [preg_match_all('/[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}/', $html), get_user_meta(1, TwoFactor::SHOW, true)], [10, '']);
check('the key is not shown again', str_contains($html, substr($setup, 0, 8)), false);
Site::$current = 2;
$_POST = ['_cg2fa_nonce' => wp_create_nonce('cloudground_2fa_profile_1'), 'cg2fa_reset' => '1'];
TwoFactor::save(1);
check('another account cannot reset it', TwoFactor::enrolled(1), true);
ob_start();
TwoFactor::profile($admin);
check('nor see the section', ob_get_clean(), '');
Site::$current = 1;
Site::$users[2] = new WP_User(2, ['read' => true], ['subscriber']);
enrol(2);
$_POST = ['_cg2fa_nonce' => wp_create_nonce('cloudground_2fa_profile_2'), 'cg2fa_reset' => '1'];
Site::$users[1]->caps['edit_user'] = true;
TwoFactor::save(2);
check('an administrator resets another account', [TwoFactor::enrolled(2), get_user_meta(2, TwoFactor::RECOVERY, true)], [false, '']);
$_POST = [];

section('reminder');
Site::reset();
Site::$current = 1;
ob_start();
TwoFactor::notice();
check('a required account signed in without an app is reminded', str_contains(ob_get_clean(), 'profile.php#cloudground-2fa'), true);
enrol(1);
ob_start();
TwoFactor::notice();
check('not once it has one', ob_get_clean(), '');

done();
