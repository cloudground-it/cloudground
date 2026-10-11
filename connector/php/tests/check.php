<?php
/**
 * The few assertions the PHP suites use. Exit status 1 when any failed.
 *
 * SPDX-License-Identifier: GPL-2.0-or-later
 */

$GLOBALS['cg_checks'] = 0;
$GLOBALS['cg_failures'] = 0;
$GLOBALS['cg_section'] = '';

function section(string $name): void
{
    $GLOBALS['cg_section'] = $name;
}

function check(string $label, $got, $want): void
{
    $GLOBALS['cg_checks']++;
    if ($got === $want) {
        return;
    }
    $GLOBALS['cg_failures']++;
    printf("FAIL [%s] %s\n  got  %s\n  want %s\n", $GLOBALS['cg_section'], $label,
        json_encode($got, JSON_UNESCAPED_SLASHES), json_encode($want, JSON_UNESCAPED_SLASHES));
}

function done(): void
{
    printf("%s: %d checks, %d failures\n", basename($_SERVER['SCRIPT_NAME']), $GLOBALS['cg_checks'], $GLOBALS['cg_failures']);
    exit($GLOBALS['cg_failures'] === 0 ? 0 : 1);
}
