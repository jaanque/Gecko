//go:build windows

package security

import (
	"syscall"
	"unsafe"
)

var (
	crypt32            = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtect   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32.NewProc("CryptUnprotectData")
	kernel32DPAPI      = syscall.NewLazyDLL("kernel32.dll")
	procLocalFree      = kernel32DPAPI.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

const dpapiMagic = "DPAPI\x01\x00"

// ProtectData encrypts plaintext using the current Windows user's DPAPI master key.
func ProtectData(plaintext []byte) ([]byte, error) {
	if len(plaintext) == 0 {
		return plaintext, nil
	}
	var inBlob dataBlob
	inBlob.cbData = uint32(len(plaintext))
	inBlob.pbData = &plaintext[0]

	var outBlob dataBlob
	r, _, err := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))

	ciphertext := unsafe.Slice(outBlob.pbData, outBlob.cbData)
	result := make([]byte, len(dpapiMagic)+len(ciphertext))
	copy(result, dpapiMagic)
	copy(result[len(dpapiMagic):], ciphertext)
	return result, nil
}

// UnprotectData decrypts DPAPI ciphertext for the current Windows user.
func UnprotectData(ciphertext []byte) ([]byte, error) {
	if len(ciphertext) <= len(dpapiMagic) || string(ciphertext[:len(dpapiMagic)]) != dpapiMagic {
		// Not DPAPI-encrypted (e.g. legacy plain JSON profile); return as-is
		return ciphertext, nil
	}
	rawCipher := ciphertext[len(dpapiMagic):]
	var inBlob dataBlob
	inBlob.cbData = uint32(len(rawCipher))
	inBlob.pbData = &rawCipher[0]

	var outBlob dataBlob
	r, _, err := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if r == 0 {
		return nil, err
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))

	result := make([]byte, outBlob.cbData)
	copy(result, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	return result, nil
}
