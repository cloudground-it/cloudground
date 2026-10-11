//go:build !unix

package main

import "errors"

func raiseFileLimit(uint64) error { return errors.New("not supported on this system") }
