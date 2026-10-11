package api

import "github.com/cloudground-it/cloudground/internal/accounts"

// The accounts functions the package's tests use to build fixtures.
var (
	hashPassword   = accounts.HashPassword
	newTOTPSecret  = accounts.NewTOTPSecret
	totpCode       = accounts.TOTPCode
	normalizeEmail = accounts.NormalizeEmail
)

const maxPasswordLen = accounts.MaxPasswordLen
