//go:build (linux && gtk4) || darwin

package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
)

type pickedCA struct {
	Ref  string `json:"ref"`
	Name string `json:"name"`
}

type dialogService struct {
	app   *application.App
	picks *caPicks
}

func (d *dialogService) PickCACert(_ context.Context) (pickedCA, error) {
	path, err := d.app.Dialog.OpenFile().
		SetTitle("Choose CA certificate").
		AddFilter("Certificates", "*.pem;*.crt").
		PromptForSingleSelection()
	if err != nil {
		return pickedCA{}, fmt.Errorf("choosing CA certificate: %w", err)
	}
	if path == "" {
		return pickedCA{}, nil
	}

	pem, err := readCAFile(path)
	if err != nil {
		return pickedCA{}, fmt.Errorf("reading CA certificate %q: %w", path, err)
	}
	return pickedCA{Ref: d.picks.add(pem), Name: filepath.Base(path)}, nil
}
