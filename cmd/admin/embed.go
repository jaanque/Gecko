package main

import (
	"embed"
	"io/fs"
)

//go:embed web/*
var webFiles embed.FS

//go:embed assets/*
var assetFiles embed.FS

func staticWebFS() fs.FS {
	sub, err := fs.Sub(webFiles, "web")
	if err != nil {
		panic(err)
	}
	return sub
}

func embeddedClientTemplate() []byte {
	data, err := assetFiles.ReadFile("assets/client_template.exe")
	if err != nil {
		return nil
	}
	return data
}
