package metadata

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"html/template"
)

const previewScript = "location.replace(document.querySelector('a').href)"

var previewPage = template.Must(template.New("preview").Parse(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><meta name="robots" content="noindex,nofollow"><title>{{.Title}}</title></head><body><a href="{{.Login}}">Sign in</a><script>` + previewScript + `</script></body></html>`))

func Preview(settings Runtime, title, login string) ([]byte, string) {
	var document bytes.Buffer
	_ = previewPage.Execute(&document, struct{ Title, Login string }{title, login})
	hash := sha256.Sum256([]byte(previewScript))
	policy := "default-src 'none'; img-src https:; script-src 'sha256-" + base64.StdEncoding.EncodeToString(hash[:]) + "'; base-uri 'none'; frame-ancestors 'none'; form-action 'none'"
	return Inherit(document.Bytes(), settings, "/"), policy
}
