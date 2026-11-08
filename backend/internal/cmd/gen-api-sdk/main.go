// Project-specific entry point for versioned caic SDK package generation.
package main

import (
	"flag"
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
	check := flag.Bool("check", false, "verify generated SDKs without writing them")
	flag.Parse()
	api := apisdkgen.NewAPI(
		"../../../../../sdk/caic", "CaicSDK",
		apisdkgen.NewVersion("v1", ".", v1.SDKAPI),
	)
	if *check {
		return api.Verify()
	}
	return api.Generate()
}
