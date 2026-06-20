// Tests for SDK output generation methods.

package apisdkgen

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/caic-xyz/caic/apisdkgen/apispec"
)

type TestJSONRPCRequest struct{}

type TestJSONRPCResponse struct{}

type TestSDKEvent struct {
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
}

type TestPathOnlyRequest struct {
	ID string `json:"-" path:"id"`
}

type TestKotlinDocumentedFields struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

func TestGenConfigGoTypeToDoc(t *testing.T) {
	t.Parallel()

	cfg := &apispec.Config{
		SpecialTypes: []apispec.SpecialType{
			{Type: reflect.TypeFor[json.RawMessage](), DocType: "JSONValue"},
			{Type: reflect.TypeFor[any](), DocType: "JSONValue"},
		},
	}
	for _, tc := range []struct {
		name string
		typ  reflect.Type
		want string
	}{
		{name: "string boolean map", typ: reflect.TypeFor[map[string]bool](), want: "Record<string, boolean>"},
		{name: "string value map", typ: reflect.TypeFor[map[string]string](), want: "Record<string, string>"},
		{name: "raw JSON map", typ: reflect.TypeFor[map[string]json.RawMessage](), want: "Record<string, JSONValue>"},
		{name: "any map", typ: reflect.TypeFor[map[string]any](), want: "Record<string, JSONValue>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := goTypeToDoc(cfg, tc.typ)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("goTypeToDoc(%s) = %q, want %q", tc.typ, got, tc.want)
			}
		})
	}
}

func TestDocRegistryGenerateKotlinMCPClient(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	docs := &docRegistry{
		cfg: &apispec.Config{
			Routes: []apispec.Route{
				{
					Name:       "mcp",
					Method:     "POST",
					Path:       "",
					Req:        reflect.TypeFor[TestJSONRPCRequest](),
					Resp:       reflect.TypeFor[TestJSONRPCResponse](),
					HeadersArg: true,
				},
			},
			KotlinPackage:      "com.example.mcp",
			MCPProtocolVersion: "2026-07-28",
			ErrorModel: apispec.ClientErrorModel{
				TypeName:      "JSONRPCResponse",
				KTCodeExpr:    "err.error?.code?.toString() ?: \"UNKNOWN\"",
				KTMessageExpr: "err.error?.message ?: \"\"",
				KTDetailsExpr: "null",
			},
		},
	}
	if err := docs.writeKotlinClient(outDir); err != nil {
		t.Fatal(err)
	}
	content, err := fs.ReadFile(os.DirFS(outDir), "ApiClient.kt")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"private val mcpID = java.util.concurrent.atomic.AtomicInteger(0)",
		"id = kotlinx.serialization.json.JsonPrimitive(mcpID.incrementAndGet())",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("ApiClient.kt does not contain %q:\n%s", want, text)
		}
	}
}

func TestDocRegistryEmitKotlinStruct(t *testing.T) {
	t.Parallel()

	t.Run("path only request", func(t *testing.T) {
		t.Parallel()

		docs := &docRegistry{}
		var b strings.Builder
		if err := docs.emitKotlinStruct(&b, reflect.TypeFor[TestPathOnlyRequest]()); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		want := "@Serializable\nclass TestPathOnlyRequest\n"
		if got != want {
			t.Fatalf("emitKotlinStruct() = %q, want %q", got, want)
		}
	})

	t.Run("field docs", func(t *testing.T) {
		t.Parallel()

		docs := &docRegistry{
			cfg: &apispec.Config{},
			fieldDoc: map[string]map[string]string{
				"TestKotlinDocumentedFields": {
					"Name": "Name is the display name.",
					"ID":   "ID is optional.",
				},
			},
		}
		var b strings.Builder
		if err := docs.emitKotlinStruct(&b, reflect.TypeFor[TestKotlinDocumentedFields]()); err != nil {
			t.Fatal(err)
		}
		got := b.String()
		for _, want := range []string{
			"    /** Name is the display name. */\n    val name: String,",
			"    /** ID is optional. */\n    val id: String? = null,",
		} {
			if !strings.Contains(got, want) {
				t.Fatalf("emitKotlinStruct() does not contain %q:\n%s", want, got)
			}
		}
	})
}

func TestDocRegistryGenerateTSValidate(t *testing.T) {
	t.Parallel()

	t.Run("without SSE routes removes stale file", func(t *testing.T) {
		t.Parallel()

		outDir := t.TempDir()
		validatePath := filepath.Join(outDir, "validate.gen.ts")
		if err := os.WriteFile(validatePath, []byte("stale"), 0o600); err != nil {
			t.Fatal(err)
		}

		docs := &docRegistry{
			cfg: &apispec.Config{},
		}
		if err := docs.generateTSValidate(outDir); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(validatePath); !os.IsNotExist(err) {
			t.Fatalf("validate.gen.ts exists after generation without SSE Routes: %v", err)
		}
	})

	t.Run("with SSE routes writes validator", func(t *testing.T) {
		t.Parallel()

		outDir := t.TempDir()
		eventType := reflect.TypeFor[TestSDKEvent]()
		docs := &docRegistry{
			cfg: &apispec.Config{
				Routes: []apispec.Route{{Name: "events", Resp: eventType, IsSSE: true}},
				SDKPackagePaths: map[string]struct{}{
					eventType.PkgPath(): {},
				},
				SSESeeds: []reflect.Type{eventType},
			},
			aliasNames: map[string]struct{}{},
		}

		if err := docs.generateTSValidate(outDir); err != nil {
			t.Fatal(err)
		}
		content, err := fs.ReadFile(os.DirFS(outDir), "validate.gen.ts")
		if err != nil {
			t.Fatal(err)
		}
		text := string(content)
		for _, want := range []string{
			"type ValidatorInput = unknown;",
			"export function validateTestSDKEvent(raw: ValidatorInput): TestSDKEvent",
		} {
			if !strings.Contains(text, want) {
				t.Fatalf("validate.gen.ts does not contain %q:\n%s", want, text)
			}
		}
	})
}
