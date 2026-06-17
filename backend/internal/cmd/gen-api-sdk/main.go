// Project-specific entry point for caic API SDK generation.
package main

import (
	"fmt"
	"os"

	"github.com/caic-xyz/caic/apisdkgen"
	"github.com/caic-xyz/caic/backend/internal/gomode"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	voicev1 "github.com/caic-xyz/caic/backend/internal/voicegateway/api/v1"
)

func main() {
	if err := mainImpl(); err != nil {
		fmt.Fprintf(os.Stderr, "gen-api-sdk: %v\n", err)
		os.Exit(1)
	}
}

func mainImpl() error {
	apis := []apisdkgen.API{
		{
			SourceDir: ".",
			Output: outputConfig(
				"../../../../../sdk/caic",
				"../../../../../sdk/caic/kotlin/src/main/kotlin/com/caic/sdk/v1",
				"../../../../../sdk/caic/swift/Sources/CaicSDK",
			),
			Config: v1.SDKAPI(),
		},
		{
			SourceDir: "../../../voicegateway/api/v1",
			Output: outputConfig(
				"../../../../../sdk/voicegateway",
				"../../../../../sdk/voicegateway/kotlin/src/main/kotlin/com/caic/voicegateway/sdk/v1",
				"../../../../../sdk/voicegateway/swift/Sources/VoiceGatewaySDK",
			),
			Config: voicev1.SDKAPI(),
		},
		{
			SourceDir: "../../../mcp",
			Output: outputConfig(
				"../../../../../sdk/mcp",
				"../../../../../sdk/mcp/kotlin/src/main/kotlin/com/fghbuild/mcp/sdk/v1",
				"../../../../../sdk/mcp/swift/Sources/MCPSDK",
			),
			Config: mcp.SDKAPI(),
		},
		{
			SourceDir: "../../../gomode",
			Output: outputConfig(
				"../../../../../sdk/gomode",
				"../../../../../sdk/gomode/kotlin/src/main/kotlin/com/fghbuild/gomode/sdk/v1",
				"../../../../../sdk/gomode/swift/Sources/GoModeSDK",
			),
			Config: gomode.SDKAPI(),
		},
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
