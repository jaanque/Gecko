//go:build !windows

package security

// ProtectData returns plaintext as-is on non-Windows platforms.
func ProtectData(plaintext []byte) ([]byte, error) {
	return plaintext, nil
}

// UnprotectData returns ciphertext as-is on non-Windows platforms.
func UnprotectData(ciphertext []byte) ([]byte, error) {
	return ciphertext, nil
}
