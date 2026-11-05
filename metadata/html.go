package metadata

import (
	"bytes"
	"fmt"
	xhtml "golang.org/x/net/html"
	"html"
	"strings"
)

func Inherit(document []byte, settings Runtime, requestPath string) []byte {
	seen := map[string]bool{}
	var title strings.Builder
	inTitle, headStarted := false, false
	offset, prefix := 0, 0
	insert := func(position int, open, close string) []byte {
		addition := open + missingHead(settings, requestPath, title.String(), seen) + close
		result := make([]byte, 0, len(document)+len(addition))
		result = append(result, document[:position]...)
		result = append(result, addition...)
		return append(result, document[position:]...)
	}
	z := xhtml.NewTokenizer(bytes.NewReader(document))
	for {
		kind := z.Next()
		raw := z.Raw()
		if kind == xhtml.ErrorToken {
			if headStarted {
				return insert(offset, "", "</head>")
			}
			return insert(prefix, "<head>", "</head>")
		}
		token := z.Token()
		switch kind {
		case xhtml.DoctypeToken:
			prefix = offset + len(raw)
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			switch token.Data {
			case "html":
				prefix = offset + len(raw)
			case "head":
				headStarted = true
			case "body":
				if headStarted {
					return insert(offset, "", "</head>")
				}
				return insert(offset, "<head>", "</head>")
			case "title":
				inTitle = true
			case "meta":
				var property, name string
				for _, attribute := range token.Attr {
					switch attribute.Key {
					case "property":
						property = attribute.Val
					case "name":
						name = attribute.Val
					}
				}
				if property != "" {
					seen["property:"+strings.ToLower(property)] = true
				}
				if name != "" {
					seen["name:"+strings.ToLower(name)] = true
				}
			case "link":
				var relations string
				for _, attribute := range token.Attr {
					if attribute.Key == "rel" {
						relations = attribute.Val
					}
				}
				for _, rel := range strings.Fields(strings.ToLower(relations)) {
					seen["rel:"+rel] = true
				}
			}
		case xhtml.TextToken:
			if inTitle {
				title.WriteString(token.Data)
			}
		case xhtml.EndTagToken:
			if token.Data == "title" {
				inTitle = false
			}
			if token.Data == "head" {
				return insert(offset, "", "")
			}
		}
		offset += len(raw)
	}
}

func missingHead(settings Runtime, requestPath, title string, seen map[string]bool) string {
	var head strings.Builder
	pageTitle := strings.TrimSpace(title)
	if pageTitle == "" {
		pageTitle = settings.Site.Name
	}
	property := func(name, value string) {
		if value != "" && !seen["property:"+name] {
			fmt.Fprintf(&head, `<meta property="%s" content="%s">`, name, html.EscapeString(value))
		}
	}
	property("og:type", "website")
	property("og:title", pageTitle)
	property("og:site_name", settings.Site.Name)
	property("og:url", settings.PageURL(requestPath))
	for _, item := range []struct {
		name  string
		media *Media
	}{{"og:image", settings.Site.Image}, {"og:video", settings.Site.Video}} {
		if item.media == nil || seen["property:"+item.name] {
			continue
		}
		property(item.name, settings.asset(item.media.URL))
		property(item.name+":type", item.media.Type)
		if item.media.Width > 0 {
			property(item.name+":width", fmt.Sprint(item.media.Width))
		}
		if item.media.Height > 0 {
			property(item.name+":height", fmt.Sprint(item.media.Height))
		}
	}
	for _, link := range []struct{ rel, href string }{{"icon", settings.Site.Icon}, {"apple-touch-icon", settings.Site.AppleTouchIcon}} {
		if link.href != "" && !seen["rel:"+link.rel] {
			fmt.Fprintf(&head, `<link rel="%s" href="%s">`, link.rel, html.EscapeString(settings.asset(link.href)))
		}
	}
	for _, theme := range []struct{ scheme, color string }{{"light", settings.Site.ThemeColor}, {"dark", settings.Site.DarkThemeColor}} {
		media := "(prefers-color-scheme: " + theme.scheme + ")"
		if theme.color != "" && !seen["name:theme-color"] {
			fmt.Fprintf(&head, `<meta name="theme-color" content="%s" media="%s">`, html.EscapeString(theme.color), media)
		}
	}
	return head.String()
}
