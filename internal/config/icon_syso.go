package config

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
)

const (
	imageFileMachineAMD64 = 0x8664
	imageFileMachineARM64 = 0xAA64

	imageScnCntInitializedData = 0x00000040
	imageScnMemRead            = 0x40000000

	imageRelAMD64Addr32NB = 3
	imageRelARM64Addr32NB = 3

	rtIcon      = 3
	rtGroupIcon = 14
)

// DefaultGeckoIconPNG generates a crisp 48x48 PNG icon with the emerald Gecko emblem.
func DefaultGeckoIconPNG() []byte {
	const size = 48
	img := image.NewRGBA(image.Rect(0, 0, size, size))

	// Draw emerald circular background
	center := size / 2
	radius := size/2 - 2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx := x - center
			dy := y - center
			distSq := dx*dx + dy*dy
			if distSq <= radius*radius {
				// Gradient from light emerald (#34d399) to dark emerald (#059669)
				factor := float64(y) / float64(size)
				r := uint8(52*(1-factor) + 5*factor)
				g := uint8(211*(1-factor) + 150*factor)
				b := uint8(153*(1-factor) + 105*factor)
				img.SetRGBA(x, y, color.RGBA{R: r, G: g, B: b, A: 255})
			}
		}
	}

	// Draw inner border highlight
	for a := 0; a < 360; a++ {
		rad := float64(radius)
		ax := int(float64(center) + rad*0.92)
		ay := int(float64(center) + rad*0.92)
		if ax >= 0 && ax < size && ay >= 0 && ay < size {
			img.SetRGBA(ax, ay, color.RGBA{R: 167, G: 243, B: 208, A: 180})
		}
	}

	// Stylized gecko spine / silhouette in white/mint
	spineColor := color.RGBA{R: 255, G: 255, B: 255, A: 240}
	eyeColor := color.RGBA{R: 6, G: 78, B: 59, A: 255}

	// Body
	for y := 14; y <= 32; y++ {
		halfW := 3
		if y < 18 || y > 28 {
			halfW = 2
		}
		for x := center - halfW; x <= center+halfW; x++ {
			img.SetRGBA(x, y, spineColor)
		}
	}
	// Head
	for y := 8; y <= 13; y++ {
		halfW := (14 - y)
		if halfW < 1 {
			halfW = 1
		}
		for x := center - halfW; x <= center+halfW; x++ {
			img.SetRGBA(x, y, spineColor)
		}
	}
	// Eyes
	img.SetRGBA(center-2, 11, eyeColor)
	img.SetRGBA(center+2, 11, eyeColor)

	// Feet pads
	drawPads := func(cx, cy int) {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				img.SetRGBA(cx+dx, cy+dy, spineColor)
			}
		}
	}
	drawPads(center-8, 16)
	drawPads(center+8, 16)
	drawPads(center-8, 28)
	drawPads(center+8, 28)

	// Tail
	for i := 0; i < 8; i++ {
		img.SetRGBA(center+i/2, 33+i, spineColor)
	}

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// GenerateResourceSyso converts a PNG icon into a COFF object file (.syso) for the given architecture.
func GenerateResourceSyso(pngBytes []byte, arch string) ([]byte, error) {
	if len(pngBytes) == 0 {
		return nil, fmt.Errorf("datos de icono vacíos")
	}

	machine := uint16(imageFileMachineAMD64)
	relType := uint16(imageRelAMD64Addr32NB)
	if arch == "arm64" {
		machine = imageFileMachineARM64
		relType = imageRelARM64Addr32NB
	}

	// Decode dimensions
	cfg, _, err := image.DecodeConfig(bytes.NewReader(pngBytes))
	w, h := 32, 32
	if err == nil && cfg.Width > 0 && cfg.Height > 0 {
		w, h = cfg.Width, cfg.Height
	}

	// GRPICONDIR + GRPICONDIRENTRY
	var grpIcon bytes.Buffer
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(0))           // idReserved
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(1))           // idType (1 = icon)
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(1))           // idCount
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint8(w))            // bWidth
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint8(h))            // bHeight
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint8(0))            // bColorCount
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint8(0))            // bReserved
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(1))           // wPlanes
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(32))          // wBitCount
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint32(len(pngBytes))) // dwBytesInRes
	_ = binary.Write(&grpIcon, binary.LittleEndian, uint16(1))           // nID
	grpBytes := grpIcon.Bytes()

	offsetRoot := 0
	offsetSub1 := 0x20
	offsetSub2 := 0x38
	offsetLang1 := 0x50
	offsetLang2 := 0x68
	offsetData1 := 0x80
	offsetData2 := 0x90
	offsetRaw1 := 0xA0
	offsetRaw2 := (offsetRaw1 + len(pngBytes) + 3) &^ 3
	totalRsrcSize := (offsetRaw2 + len(grpBytes) + 3) &^ 3

	rsrc := make([]byte, totalRsrcSize)

	// Root dir
	binary.LittleEndian.PutUint16(rsrc[offsetRoot+14:], 2)
	binary.LittleEndian.PutUint32(rsrc[offsetRoot+16:], rtIcon)
	binary.LittleEndian.PutUint32(rsrc[offsetRoot+20:], uint32(offsetSub1)|0x80000000)
	binary.LittleEndian.PutUint32(rsrc[offsetRoot+24:], rtGroupIcon)
	binary.LittleEndian.PutUint32(rsrc[offsetRoot+28:], uint32(offsetSub2)|0x80000000)

	// SubDir 1 (RT_ICON)
	binary.LittleEndian.PutUint16(rsrc[offsetSub1+14:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetSub1+16:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetSub1+20:], uint32(offsetLang1)|0x80000000)

	// SubDir 2 (RT_GROUP_ICON)
	binary.LittleEndian.PutUint16(rsrc[offsetSub2+14:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetSub2+16:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetSub2+20:], uint32(offsetLang2)|0x80000000)

	// LangDir 1
	binary.LittleEndian.PutUint16(rsrc[offsetLang1+14:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetLang1+16:], 0x0409) // en-US
	binary.LittleEndian.PutUint32(rsrc[offsetLang1+20:], uint32(offsetData1))

	// LangDir 2
	binary.LittleEndian.PutUint16(rsrc[offsetLang2+14:], 1)
	binary.LittleEndian.PutUint32(rsrc[offsetLang2+16:], 0x0409)
	binary.LittleEndian.PutUint32(rsrc[offsetLang2+20:], uint32(offsetData2))

	// DataEntry 1
	binary.LittleEndian.PutUint32(rsrc[offsetData1:], uint32(offsetRaw1))
	binary.LittleEndian.PutUint32(rsrc[offsetData1+4:], uint32(len(pngBytes)))

	// DataEntry 2
	binary.LittleEndian.PutUint32(rsrc[offsetData2:], uint32(offsetRaw2))
	binary.LittleEndian.PutUint32(rsrc[offsetData2+4:], uint32(len(grpBytes)))

	// Copy raw payloads
	copy(rsrc[offsetRaw1:], pngBytes)
	copy(rsrc[offsetRaw2:], grpBytes)

	// Relocations (2 relocations for the data offsets)
	var relocs bytes.Buffer
	_ = binary.Write(&relocs, binary.LittleEndian, uint32(offsetData1))
	_ = binary.Write(&relocs, binary.LittleEndian, uint32(0)) // Symbol index 0
	_ = binary.Write(&relocs, binary.LittleEndian, relType)

	_ = binary.Write(&relocs, binary.LittleEndian, uint32(offsetData2))
	_ = binary.Write(&relocs, binary.LittleEndian, uint32(0))
	_ = binary.Write(&relocs, binary.LittleEndian, relType)
	relocBytes := relocs.Bytes()

	offsetSectionHdr := 20
	offsetRawData := offsetSectionHdr + 40
	offsetRelocs := offsetRawData + len(rsrc)
	offsetSymbols := offsetRelocs + len(relocBytes)

	var out bytes.Buffer

	// COFF Header (20 bytes)
	_ = binary.Write(&out, binary.LittleEndian, machine)
	_ = binary.Write(&out, binary.LittleEndian, uint16(1)) // Number of sections
	_ = binary.Write(&out, binary.LittleEndian, uint32(0)) // TimeDateStamp
	_ = binary.Write(&out, binary.LittleEndian, uint32(offsetSymbols))
	_ = binary.Write(&out, binary.LittleEndian, uint32(1)) // 1 symbol
	_ = binary.Write(&out, binary.LittleEndian, uint16(0)) // SizeOfOptionalHeader
	_ = binary.Write(&out, binary.LittleEndian, uint16(0x0104))

	// Section Header (40 bytes)
	var secName [8]byte
	copy(secName[:], ".rsrc")
	_ = binary.Write(&out, binary.LittleEndian, secName)
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))                 // VirtualSize
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))                 // VirtualAddress
	_ = binary.Write(&out, binary.LittleEndian, uint32(len(rsrc)))         // SizeOfRawData
	_ = binary.Write(&out, binary.LittleEndian, uint32(offsetRawData))     // PointerToRawData
	_ = binary.Write(&out, binary.LittleEndian, uint32(offsetRelocs))      // PointerToRelocations
	_ = binary.Write(&out, binary.LittleEndian, uint32(0))                 // PointerToLinenumbers
	_ = binary.Write(&out, binary.LittleEndian, uint16(2))                 // NumberOfRelocations
	_ = binary.Write(&out, binary.LittleEndian, uint16(0))                 // NumberOfLinenumbers
	_ = binary.Write(&out, binary.LittleEndian, uint32(imageScnCntInitializedData|imageScnMemRead))

	// Section Raw Data
	out.Write(rsrc)

	// Relocations
	out.Write(relocBytes)

	// Symbol Table (18 bytes for symbol 0)
	var symName [8]byte
	copy(symName[:], ".rsrc")
	_ = binary.Write(&out, binary.LittleEndian, symName)
	_ = binary.Write(&out, binary.LittleEndian, uint32(0)) // Value
	_ = binary.Write(&out, binary.LittleEndian, int16(1))  // SectionNumber = 1
	_ = binary.Write(&out, binary.LittleEndian, uint16(0)) // Type
	_ = binary.Write(&out, binary.LittleEndian, uint8(3))  // StorageClass = STATIC
	_ = binary.Write(&out, binary.LittleEndian, uint8(0))  // NumberOfAuxSymbols

	// String Table (minimum 4 bytes for length)
	_ = binary.Write(&out, binary.LittleEndian, uint32(4))

	return out.Bytes(), nil
}

// EnsurePackageResources generates resource_windows_amd64.syso and resource_windows_arm64.syso in pkgDir.
func EnsurePackageResources(pkgDir string, pngBytes []byte) error {
	if len(pngBytes) == 0 {
		pngBytes = DefaultGeckoIconPNG()
	}
	for _, arch := range []string{"amd64", "arm64"} {
		syso, err := GenerateResourceSyso(pngBytes, arch)
		if err != nil {
			return err
		}
		dest := filepath.Join(pkgDir, fmt.Sprintf("resource_windows_%s.syso", arch))
		if err := os.WriteFile(dest, syso, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// EnsureDefaultIcons ensures all main packages have their Windows resource .syso files in place.
func EnsureDefaultIcons() error {
	png := DefaultGeckoIconPNG()
	for _, pkg := range []string{"cmd/admin", "cmd/client", "cmd/relay"} {
		if err := EnsurePackageResources(pkg, png); err != nil {
			return err
		}
	}
	return nil
}
