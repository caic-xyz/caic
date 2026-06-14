// Package apisdkgen generates typed API SDKs and API reference documents.
package apisdkgen

import (
	"errors"
	"fmt"
)

// OutputConfig names the generated output directories for each target.
type OutputConfig struct {
	TypeScriptDir string
	KotlinDir     string
	SwiftDir      string
	MarkdownDir   string
}

// API describes one API surface to generate.
type API struct {
	SourceDir string
	Output    OutputConfig
	Config    Config
}

// Generate writes configured SDK outputs for one API surface.
func Generate(api *API) error {
	if api == nil {
		return errors.New("api is nil")
	}
	docs, err := loadDocsInDir(api.SourceDir)
	if err != nil {
		return fmt.Errorf("loading docs: %w", err)
	}
	docs.cfg = &api.Config

	if api.Output.TypeScriptDir != "" {
		if err := docs.generateTSTypes(api.Output.TypeScriptDir); err != nil {
			return err
		}
		if err := docs.generateTS(api.Output.TypeScriptDir); err != nil {
			return err
		}
		if err := docs.generateTSValidate(api.Output.TypeScriptDir); err != nil {
			return err
		}
	}
	if api.Output.KotlinDir != "" {
		if err := docs.generateKotlin(api.Output.KotlinDir); err != nil {
			return err
		}
	}
	if api.Output.SwiftDir != "" {
		if err := docs.generateSwift(api.Output.SwiftDir); err != nil {
			return err
		}
	}
	if api.Output.MarkdownDir != "" {
		if err := docs.generateMarkdownDoc(api.Output.MarkdownDir); err != nil {
			return err
		}
	}
	return nil
}
