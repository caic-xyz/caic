# apisdkgen

Generates typed SDKs (TypeScript, Kotlin, Swift) and API reference docs from Go DTO types and route definitions.

## Usage

Define an `SDKAPI() apispec.Config[ErrorCode]` function in your package, then run:

```bash
go run ./backend/internal/cmd/gen-api-sdk
```

Generated SDKs land in `sdk/<pkg>/ts/`, `sdk/<pkg>/kotlin/`, and `sdk/<pkg>/swift/`.

## SDK Spec

```go
func SDKAPI() apispec.Config[ErrorCode] {
    return apispec.Config[ErrorCode]{
        ExtraSeeds:    []reflect.Type{...}, // DTO types to generate
        KotlinPackage: "...",
        APIDocTitle:   "...",
    }
}
```

APIs without an error-code enum use `apispec.Config[string]`.

See `apispec/apispec.go` for the full `Config` type.
