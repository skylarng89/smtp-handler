package render

import (
	"strings"

	"golang.org/x/net/html"
)

// blockTags force a line break before and after their content.
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "tr": true, "li": true, "ul": true, "ol": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"table": true, "blockquote": true, "pre": true, "hr": true, "section": true, "article": true,
}

// HTMLToText derives a readable plain-text alternative from an HTML body.
// Links keep their target as "text (url)"; script/style content is dropped.
func HTMLToText(src string) string {
	z := html.NewTokenizer(strings.NewReader(src))

	var (
		b        strings.Builder
		skip     int // depth inside script/style/head
		hrefs    []string
		linkText []int // builder length at the start of each open <a>
	)
	newline := func() {
		s := b.String()
		if s != "" && !strings.HasSuffix(s, "\n") {
			b.WriteByte('\n')
		}
	}

	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			// io.EOF is the normal end; any other error means malformed
			// HTML, where returning what was extracted so far is best.
			break
		}
		switch tt {
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "head", "title":
				if tt == html.StartTagToken {
					skip++
				}
			case "a":
				href := ""
				for hasAttr {
					var k, v []byte
					k, v, hasAttr = z.TagAttr()
					if string(k) == "href" {
						href = string(v)
					}
				}
				hrefs = append(hrefs, href)
				linkText = append(linkText, b.Len())
			case "li":
				newline()
				b.WriteString("- ")
			default:
				if blockTags[tag] {
					newline()
				}
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch tag {
			case "script", "style", "head", "title":
				if skip > 0 {
					skip--
				}
			case "a":
				if n := len(hrefs); n > 0 {
					href, start := hrefs[n-1], linkText[n-1]
					hrefs, linkText = hrefs[:n-1], linkText[:n-1]
					text := strings.TrimSpace(b.String()[start:])
					if href != "" && !strings.HasPrefix(href, "#") && text != href &&
						(strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") || strings.HasPrefix(href, "mailto:")) {
						b.WriteString(" (")
						b.WriteString(href)
						b.WriteByte(')')
					}
				}
			default:
				if blockTags[tag] {
					newline()
				}
			}
		case html.TextToken:
			if skip > 0 {
				continue
			}
			text := collapseSpace(string(z.Text()))
			if text == "" {
				continue
			}
			s := b.String()
			if s != "" && !strings.HasSuffix(s, "\n") && !strings.HasSuffix(s, " ") && !strings.HasPrefix(text, " ") {
				b.WriteByte(' ')
			}
			if strings.HasSuffix(s, "\n") || s == "" {
				text = strings.TrimLeft(text, " ")
			}
			b.WriteString(text)
		}
	}
	return strings.TrimSpace(tidyBlankLines(b.String())) + "\n"
}

func collapseSpace(s string) string {
	return strings.Join(strings.Fields(s), " ") + trailingSpace(s)
}

func trailingSpace(s string) string {
	if s != "" && strings.TrimSpace(s) != "" && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t') {
		return " "
	}
	return ""
}

// tidyBlankLines trims trailing spaces and limits consecutive blank lines to one.
func tidyBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " ")
		if ln == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.Join(out, "\n")
}
