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

type TestSSEError struct {
	Message string `json:"message"`
}

type TestPathOnlyRequest struct {
	ID string `json:"-" path:"id"`
}

type TestKotlinDocumentedFields struct {
	Name string `json:"name"`
	ID   string `json:"id,omitempty"`
}

type TestDocAuthKind string

const (
	TestDocAuthKindOAuth  TestDocAuthKind = "oauth"
	TestDocAuthKindAPIKey TestDocAuthKind = "apikey"
)

type TestDocProviderQuota struct {
	AuthKind TestDocAuthKind `json:"authKind"`
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

func TestDocRegistryGenerateMarkdownDoc(t *testing.T) {
	t.Parallel()

	t.Run("enum fields retain their type and values", func(t *testing.T) {
		t.Parallel()
		outDir := t.TempDir()
		quotaType := reflect.TypeFor[TestDocProviderQuota]()
		docs := &docRegistry{
			cfg: &apispec.Config{
				APIDocTitle:     "Test API",
				SDKPackagePaths: map[string]struct{}{quotaType.PkgPath(): {}},
				Routes: []apispec.Route{{
					Name:   "quota",
					Method: "GET",
					Path:   "/quota",
					Resp:   quotaType,
				}},
			},
			typeDoc: map[string]string{"TestDocAuthKind": "TestDocAuthKind identifies an authentication method."},
			aliases: []aliasInfo{{
				name: "TestDocAuthKind",
				constants: []aliasConstant{
					{name: "TestDocAuthKindOAuth", value: "oauth"},
					{name: "TestDocAuthKindAPIKey", value: "apikey", doc: "API key credentials."},
				},
			}},
		}
		if err := docs.generateMarkdownDoc(outDir); err != nil {
			t.Fatal(err)
		}
		data, err := fs.ReadFile(os.DirFS(outDir), "API.md")
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		for _, want := range []string{
			"### TestDocAuthKind",
			"| `oauth` |  |",
			"| `apikey` | API key credentials. |",
			"| `authKind` | `TestDocAuthKind` |  | yes |",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("API.md does not contain %q:\n%s", want, text)
			}
		}
	})
}

func TestLoadDocsInDir(t *testing.T) {
	t.Parallel()

	t.Run("string alias docs", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		source := `package v1

// TestAuthKind identifies an authentication method.
type TestAuthKind string

const (
	// TestAuthKindOAuth uses OAuth credentials.
	TestAuthKindOAuth TestAuthKind = "oauth"
)
`
		if err := os.WriteFile(filepath.Join(dir, "types.go"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		docs, err := loadDocsInDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := docs.typeDoc["TestAuthKind"]; got != "TestAuthKind identifies an authentication method." {
			t.Errorf("string alias doc = %q", got)
		}
		if got := docs.aliases[0].constants[0].doc; got != "TestAuthKindOAuth uses OAuth credentials." {
			t.Errorf("string alias value doc = %q", got)
		}
	})
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

func TestDocRegistryGenerateTSNamedEvents(t *testing.T) {
	t.Parallel()

	outDir := t.TempDir()
	docs := &docRegistry{
		cfg: &apispec.Config{
			Routes: []apispec.Route{
				{
					Name:  "events",
					Path:  "/events",
					Resp:  reflect.TypeFor[TestSDKEvent](),
					IsSSE: true,
					SSEEvents: []apispec.SSEEvent{
						{Name: "ready", Handler: "onReady"},
						{Name: "reset", Handler: "onReset"},
						{Name: "error", Handler: "onHistoryError", Resp: reflect.TypeFor[TestSSEError]()},
					},
				},
				{Name: "rawEvents", Path: "/raw-events", Resp: reflect.TypeFor[TestSDKEvent](), IsSSE: true},
			},
			ErrorModel: apispec.ClientErrorModel{TypeName: "DifferentError"},
		},
	}
	if err := docs.generateTS(outDir); err != nil {
		t.Fatal(err)
	}
	content, err := fs.ReadFile(os.DirFS(outDir), "api.gen.ts")
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		"import type { DifferentError, TestSDKEvent, TestSSEError } from \"./types.gen\";",
		"export interface EventsHandlers {",
		"export interface RawEventsHandlers {",
		"onMessage: (event: TestSDKEvent) => void;",
		"onError: (err: unknown) => void;",
		"onReady?: () => void;",
		"onReset?: () => void;",
		"onHistoryError?: (event: TestSSEError) => void;",
		"events: (handlers: EventsHandlers): EventSource => {",
		"rawEvents: (handlers: RawEventsHandlers): EventSource => {",
		"handlers.onMessage(validateTestSDKEvent(JSON.parse(e.data)));",
		"if (!(e instanceof MessageEvent) || typeof e.data !== \"string\") return;",
		"handlers.onHistoryError?.(validateTestSSEError(JSON.parse(e.data)));",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("api.gen.ts does not contain %q:\n%s", want, text)
		}
	}

	if err := docs.generateMarkdownDoc(outDir); err != nil {
		t.Fatal(err)
	}
	content, err = fs.ReadFile(os.DirFS(outDir), "API.md")
	if err != nil {
		t.Fatal(err)
	}
	want := "`TestSDKEvent` SSE<br>Named events: `ready`, `reset`, `error` (`TestSSEError`)"
	if !strings.Contains(string(content), want) {
		t.Errorf("API.md does not contain %q:\n%s", want, content)
	}
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
