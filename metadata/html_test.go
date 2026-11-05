package metadata_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/holon-sh/runtime/metadata"
)

func TestHTMLInheritsSiteMetadataWithoutReplacingPageOverrides(t *testing.T) {
	settings := metadata.Runtime{Origin: "https://tenant.example", Mount: "/manuals", Site: metadata.Site{
		Name: "Example", Icon: "/favicon.svg", AppleTouchIcon: "/touch.png", ThemeColor: "#ffffff", DarkThemeColor: "#000000",
		Image: &metadata.Media{URL: "/social.png", Type: "image/png", Width: 1200, Height: 630},
		Video: &metadata.Media{URL: "https://cdn.example/movie.mp4", Type: "video/mp4", Width: 1200, Height: 630},
	}}
	for _, test := range []struct {
		name, head       string
		expected, absent []string
	}{
		{"defaults", `<title>Manual &amp; reference</title><script>const text="</head>";</script>`, []string{
			`property="og:title" content="Manual &amp; reference"`, `property="og:url" content="https://tenant.example/manuals/topic.html"`,
			`property="og:image" content="https://tenant.example/social.png"`, `property="og:video" content="https://cdn.example/movie.mp4"`,
			`property="og:image:width" content="1200"`, `rel="icon" href="https://tenant.example/favicon.svg"`,
			`name="theme-color" content="#000000" media="(prefers-color-scheme: dark)"`,
		}, nil},
		{"partial theme", `<title>App</title><meta name="theme-color" content="red" media="(prefers-color-scheme:light)">`, []string{`name="theme-color" content="#000000" media="(prefers-color-scheme: dark)"`}, []string{"#ffffff"}},
		{"overrides", `<title>Manual</title><meta property="og:title" content="Custom"><meta property="og:image" content="/own.png"><link rel="icon" href="/own.svg"><meta name="theme-color" content="red">`, []string{
			`property="og:title" content="Custom"`, `property="og:image" content="/own.png"`, `rel="icon" href="/own.svg"`,
			`property="og:video" content="https://cdn.example/movie.mp4"`,
		}, []string{"social.png", "og:image:width", "favicon.svg", "#ffffff", "#000000"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := `<!doctype html><html><head>` + test.head + `</head><body><h1>Private app</h1></body></html>`
			output := string(metadata.Inherit([]byte(input), settings, "/topic.html"))
			for _, expected := range test.expected {
				if !strings.Contains(output, expected) {
					t.Errorf("missing %s in %s", expected, output)
				}
			}
			for _, absent := range test.absent {
				if strings.Contains(output, absent) {
					t.Errorf("unexpected inherited override %s", absent)
				}
			}
			if !strings.Contains(output, test.head) || !strings.HasSuffix(output, `<body><h1>Private app</h1></body></html>`) {
				t.Fatal("app HTML was rewritten")
			}
			if twice := metadata.Inherit([]byte(output), settings, "/topic.html"); !bytes.Equal(twice, []byte(output)) {
				t.Fatal("inheritance duplicated tags")
			}
		})
	}
}

func TestSiteConfigurationRejectsUnsafeAssetReferences(t *testing.T) {
	for _, asset := range []string{"javascript:alert(1)", "//other.example/image.png", "relative.png", "https://user:password@example.test/image.png"} {
		if _, err := metadata.Parse(`{"icon":"` + asset + `"}`); err == nil {
			t.Errorf("accepted %s", asset)
		}
	}
	if _, err := metadata.Parse(`{"unknown":true}`); err == nil {
		t.Fatal("accepted unknown site field")
	}
}

func TestHTMLWithImplicitHeadInheritsMetadata(t *testing.T) {
	settings := metadata.Runtime{Origin: "https://preview.example", AssetOrigin: "https://public.example", Mount: "/app", Site: metadata.Site{Name: "Brand", Icon: "/icon.svg"}}
	for _, document := range []string{
		`<h1>Application</h1>`,
		`<!doctype html><html><body><h1>Application</h1></body></html>`,
		`<html><head><title>Application</title><body><h1>Application</h1></body></html>`,
	} {
		output := metadata.Inherit([]byte(document), settings, "/")
		if !strings.Contains(string(output), `property="og:url" content="https://preview.example/app/"`) || !strings.Contains(string(output), `href="https://public.example/icon.svg"`) || !strings.Contains(string(output), `<h1>Application</h1>`) {
			t.Fatalf("missing defaults or lost app content: %s", output)
		}
	}
}
