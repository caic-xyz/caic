// Project-specific entry point for caic API SDK generation.
package main

import (
	"fmt"
	"os"

	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/maruel/apisdkgen"
)

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-api-sdk: %v\n", err)
		os.Exit(1)
	}
}

func mainImpl() error {
	apis := []apisdkgen.API{
		apisdkgen.NewAPI(
			".",
			outputConfig(
				"../../../../../sdk/caic",
				"../../../../../sdk/caic/kotlin/src/main/kotlin/com/caic/sdk/v1",
				"../../../../../sdk/caic/swift/Sources/CaicSDK",
			),
			v1.SDKAPI(),
		),
	}
	for i := range apis {
		if err := apisdkgen.Generate(&apis[i]); err != nil {
			return err
		}
	}
	return nil
}

func outputConfig(sdkDir, kotlinDir, swiftDir string) apisdkgen.OutputConfig {
	return apisdkgen.OutputConfig{
		TypeScriptDir: sdkDir + "/ts/v1",
		KotlinDir:     kotlinDir,
		SwiftDir:      swiftDir,
		MarkdownDir:   sdkDir,
	}
}
