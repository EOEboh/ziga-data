package ingest

import (
	"regexp"
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// dropSubtrees are elements whose contents are never readable text. Dropping
// them wholesale matters for cost as much as accuracy: a marketing email's
// <style> block alone can be several kilobytes of tokens.
var dropSubtrees = map[atom.Atom]bool{
	atom.Script:   true,
	atom.Style:    true,
	atom.Head:     true,
	atom.Title:    true,
	atom.Noscript: true,
}

// blockElements end a line when they close, so a table of contact details does
// not collapse into one run-on sentence.
var blockElements = map[atom.Atom]bool{
	atom.P: true, atom.Div: true, atom.Tr: true, atom.Li: true, atom.Br: true,
	atom.H1: true, atom.H2: true, atom.H3: true, atom.H4: true, atom.H5: true,
	atom.H6: true, atom.Blockquote: true, atom.Table: true, atom.Ul: true,
	atom.Ol: true, atom.Section: true, atom.Article: true, atom.Header: true,
	atom.Footer: true, atom.Hr: true,
}

var (
	manyNewlines  = regexp.MustCompile(`\n{3,}`)
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	manySpaces    = regexp.MustCompile(`[ \t]{2,}`)
	looksLikeURL  = regexp.MustCompile(`^\s*(https?://|www\.)`)
)

// HTMLToText renders an HTML message part as readable text.
//
// This exists for two reasons at once. Accuracy: an HTML-only message has no
// plain-text part to extract from. And cost: naively feeding markup to the
// model spends tokens on attributes, tracking pixels and style rules, none of
// which contain a lead.
//
// It parses rather than pattern-matching, because regex-stripping HTML fails
// on exactly the messages that matter — the ones a real mail client generated.
func HTMLToText(source string) string {
	doc, err := html.Parse(strings.NewReader(source))
	if err != nil {
		// html.Parse is extremely tolerant, so this is close to unreachable;
		// returning the raw source is still better than returning nothing.
		return strings.TrimSpace(source)
	}

	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && dropSubtrees[n.DataAtom] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		if n.Type == html.ElementNode && n.DataAtom == atom.A {
			// Render a link as its text, falling back to the href when the
			// text is empty or is itself a URL. A tracking link wrapped around
			// a word should read as the word; a bare URL the sender typed is
			// itself the content and must survive.
			if text := strings.TrimSpace(nodeText(n)); text != "" && !looksLikeURL.MatchString(text) {
				b.WriteString(text)
				b.WriteString("\n")
				return
			}
			if href := attr(n, "href"); href != "" && !strings.HasPrefix(href, "#") {
				b.WriteString(href)
				b.WriteString("\n")
				return
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
		if n.Type == html.ElementNode && blockElements[n.DataAtom] {
			b.WriteString("\n")
		}
	}
	walk(doc)

	return tidy(b.String())
}

// tidy collapses the whitespace HTML rendering inevitably produces.
func tidy(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, " ", " ") // NBSP is pervasive in mail HTML
	s = strings.ReplaceAll(s, "​", "")  // zero-width space, used by trackers
	s = manySpaces.ReplaceAllString(s, " ")
	s = trailingSpace.ReplaceAllString(s, "\n")
	s = manyNewlines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && dropSubtrees[n.DataAtom] {
			return
		}
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}
