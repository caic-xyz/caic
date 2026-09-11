// Package apisdkgen generates typed SDKs (TypeScript, Kotlin, Swift) and API
// reference documents from Go DTO types and route definitions.
//
// SDK specifications are defined by each package in an exported SDKAPI()
// function returning an apispec.Config with that API's error-code type. The
// gen-api-sdk command discovers
// these specs and produces output in sdk/<pkg>/<lang>/.
package apisdkgen

import (
	"errors"
	"fmt"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
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
	generate func() error
}

// NewAPI constructs an SDK generation target from a typed API specification.
func NewAPI[C ~string](sourceDir string, output OutputConfig, config apispec.Config[C]) API {
	return API{generate: func() error {
		return generateAPI(sourceDir, output, config)
	}}
}

// Generate writes configured SDK outputs for one API surface.
func Generate(api *API) error {
	if api == nil {
		return errors.New("api is nil")
	}
	if api.generate == nil {
		return errors.New("api has no generator")
	}
	return api.generate()
}

func generateAPI[C ~string](sourceDir string, output OutputConfig, config apispec.Config[C]) error {
	docs, err := loadDocsInDir[C](sourceDir)
	if err != nil {
		return fmt.Errorf("loading docs: %w", err)
	}
	docs.cfg = &config
	if err := docs.addConfiguredErrorCodeAlias(); err != nil {
		return fmt.Errorf("configuring error-code type: %w", err)
	}

	if output.TypeScriptDir != "" {
		if err := docs.generateTSTypes(output.TypeScriptDir); err != nil {
			return err
		}
		if err := docs.generateTS(output.TypeScriptDir); err != nil {
			return err
		}
		if err := docs.generateTSValidate(output.TypeScriptDir); err != nil {
			return err
		}
	}
	if output.KotlinDir != "" {
		if err := docs.generateKotlin(output.KotlinDir); err != nil {
			return err
		}
	}
	if output.SwiftDir != "" {
		if err := docs.generateSwift(output.SwiftDir); err != nil {
			return err
		}
	}
	if output.MarkdownDir != "" {
		if err := docs.generateMarkdownDoc(output.MarkdownDir); err != nil {
			return err
		}
	}
	return nil
}
