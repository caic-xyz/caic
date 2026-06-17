# apisdkgen

Generates typed SDKs (TypeScript, Kotlin, Swift) and API reference docs from Go DTO types and route definitions.

## Usage

Define an `SDKAPI() apispec.Config` function in your package, then run:

```bash
go run ./backend/internal/cmd/gen-api-sdk
```

Generated SDKs land in `sdk/<pkg>/ts/`, `sdk/<pkg>/kotlin/`, and `sdk/<pkg>/swift/`.

## SDK Spec

```go
func SDKAPI() apispec.Config {
    return apispec.Config{
        ExtraSeeds:    []reflect.Type{...}, // DTO types to generate
        KotlinPackage: "...",
        APIDocTitle:   "...",
    }
}
```

See `apispec/apispec.go` for the full `Config` type.
