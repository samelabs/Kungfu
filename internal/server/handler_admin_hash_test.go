package server

import "kungfu.md/internal/auth"

func hashPasswordForAdminTest(password string) (string, error) {
	return auth.HashPassword(password)
}
