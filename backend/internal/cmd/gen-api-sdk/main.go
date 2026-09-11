// Project-specific entry point for caic API SDK generation.
package main

import (
	"fmt"
	"os"

	"github.com/caic-xyz/caic/apisdkgen"
	"github.com/caic-xyz/caic/backend/internal/mcp"
	v1 "github.com/caic-xyz/caic/backend/internal/server/api/v1"
	"github.com/caic-xyz/caic/gomode"
	voicev1 "github.com/caic-xyz/caic/gomode/voicegateway/api/v1"
	"github.com/caic-xyz/caic/oauth"
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
		apisdkgen.NewAPI(
			"../../../../../gomode/voicegateway/api/v1",
			outputConfig(
				"../../../../../sdk/voicegateway",
				"../../../../../sdk/voicegateway/kotlin/src/main/kotlin/com/caic/voicegateway/sdk/v1",
				"../../../../../sdk/voicegateway/swift/Sources/VoiceGatewaySDK",
			),
			voicev1.SDKAPI(),
		),
		apisdkgen.NewAPI(
			"../../../mcp",
			outputConfig(
				"../../../../../sdk/mcp",
				"../../../../../sdk/mcp/kotlin/src/main/kotlin/com/fghbuild/mcp/sdk/v1",
				"../../../../../sdk/mcp/swift/Sources/MCPSDK",
			),
			mcp.SDKAPI(),
		),
		apisdkgen.NewAPI(
			"../../../../../gomode",
			outputConfig(
				"../../../../../sdk/gomode",
				"../../../../../sdk/gomode/kotlin/src/main/kotlin/com/fghbuild/gomode/sdk/v1",
				"../../../../../sdk/gomode/swift/Sources/GoModeSDK",
			),
			gomode.SDKAPI(),
		),
		apisdkgen.NewAPI(
			"../../../../../oauth",
			outputConfig(
				"../../../../../sdk/oauth",
				"../../../../../sdk/oauth/kotlin/src/main/kotlin/com/caic/oauth/sdk/v1",
				"../../../../../sdk/oauth/swift/Sources/OAuthSDK",
			),
			oauth.SDKAPI(),
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
