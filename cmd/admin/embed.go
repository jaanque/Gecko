package main

import "embed"

//go:embed assets/*
var assetFiles embed.FS

func embeddedClientTemplate() []byte {
	data, err := assetFiles.ReadFile("assets/client_template.exe")
	if err != nil {
		return nil
	}
	return data
}

func embeddedLinuxClientTemplate() []byte {
	data, err := assetFiles.ReadFile("assets/client_template_linux")
	if err != nil {
		return nil
	}
	return data
}
