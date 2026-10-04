// Command termshot renders text from standard input as an SVG picture of a
// terminal window.
//
// It is how the screenshots under docs/screenshots are produced: each one is
// the captured output of a real command (see scripts/screenshots.sh), so they
// can be regenerated instead of going stale, and they stay sharp and diffable
// because SVG is text.
//
//	some-command | termshot -title "some-command" -o out.svg
package main

import (
	"bufio"
	"flag"
	"fmt"
	"html"
	"os"
	"strings"
	"unicode/utf8"
)

const (
	fontSize   = 14
	charWidth  = 8.43 // advance of a 14px monospace glyph
	lineHeight = 20
	padding    = 18
	titleBar   = 34
)

// lineColor picks a colour from the shape of the line, which is enough to
// make psql and test output readable at a glance.
func lineColor(line string) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(trimmed, "$ "), strings.HasPrefix(trimmed, "--- PASS"):
		return "#7ee787"
	case strings.HasPrefix(trimmed, "ERROR:"), strings.HasPrefix(trimmed, "FAIL"), strings.HasPrefix(trimmed, "--- FAIL"):
		return "#ff7b72"
	case strings.HasPrefix(trimmed, "DETAIL:"), strings.HasPrefix(trimmed, "LINE "), trimmed == "^":
		return "#ffa198"
	case strings.HasPrefix(trimmed, "--"), strings.HasPrefix(trimmed, "#"):
		return "#8b949e"
	case strings.HasPrefix(trimmed, "ok "), strings.HasPrefix(trimmed, "PASS"), strings.HasPrefix(trimmed, "all checks passed"):
		return "#7ee787"
	}
	return "#e6edf3"
}

func main() {
	title := flag.String("title", "", "text shown in the title bar")
	out := flag.String("o", "", "output file (default: standard output)")
	flag.Parse()

	var lines []string
	cols := utf8.RuneCountInString(*title) + 12
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimRight(strings.ReplaceAll(sc.Text(), "\t", "    "), " \r")
		lines = append(lines, line)
		cols = max(cols, utf8.RuneCountInString(line))
	}
	if err := sc.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "termshot:", err)
		os.Exit(1)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	width := int(float64(cols)*charWidth) + 2*padding
	height := titleBar + len(lines)*lineHeight + 2*padding

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-label="%s">`+"\n",
		width, height, width, height, html.EscapeString(*title))
	fmt.Fprintf(&sb, `<rect width="%d" height="%d" rx="10" fill="#0d1117"/>`+"\n", width, height)
	fmt.Fprintf(&sb, `<path d="M0 10a10 10 0 0 1 10-10h%d a10 10 0 0 1 10 10v%d h-%d z" fill="#161b22"/>`+"\n",
		width-20, titleBar-10, width)
	for i, color := range []string{"#ff5f56", "#ffbd2e", "#27c93f"} {
		fmt.Fprintf(&sb, `<circle cx="%d" cy="%d" r="6" fill="%s"/>`+"\n", 20+i*20, titleBar/2, color)
	}
	font := `font-family="ui-monospace, SFMono-Regular, Menlo, Consolas, 'Liberation Mono', monospace"`
	fmt.Fprintf(&sb, `<text x="%d" y="%d" text-anchor="middle" %s font-size="13" fill="#8b949e">%s</text>`+"\n",
		width/2, titleBar/2+5, font, html.EscapeString(*title))
	fmt.Fprintf(&sb, `<g %s font-size="%d">`+"\n", font, fontSize)
	for i, line := range lines {
		if line == "" {
			continue
		}
		y := titleBar + padding + i*lineHeight + fontSize
		// SVG collapses runs of spaces, which would wreck psql's column
		// alignment and error carets. No-break spaces are never collapsed
		// and are as wide as a space in a monospace font.
		text := strings.ReplaceAll(html.EscapeString(line), " ", " ")
		fmt.Fprintf(&sb, `<text x="%d" y="%d" fill="%s">%s</text>`+"\n", padding, y, lineColor(line), text)
	}
	sb.WriteString("</g>\n</svg>\n")

	if *out == "" {
		fmt.Print(sb.String())
		return
	}
	if err := os.WriteFile(*out, []byte(sb.String()), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "termshot:", err)
		os.Exit(1)
	}
}
