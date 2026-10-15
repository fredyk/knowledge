package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// DOG sections HTML. Stable URLs of the form
//
//	https://www.xunta.gal/dog/Publicados/<YYYY>/<YYYYMMDD>/Secciones<N>_es.html
//
// One file per section (1..5), each contains a list of items with their html
// page and pdf url plus the title text.
const dogSectionURL = "https://www.xunta.gal/dog/Publicados/%s/%s/Secciones%d_es.html"

var (
	// Captures `<p class="dog-toc-nivel-1">III. Otras disposiciones</p>`
	reDogSection = regexp.MustCompile(`(?s)<p class="dog-toc-nivel-1">([^<]+)</p>`)
	// Captures `<p class="dog-toc-organismo">Conselleria de XYZ</p>`
	reDogOrg = regexp.MustCompile(`(?s)<p class="dog-toc-organismo">([^<]+)</p>`)
	// Captures `<li class="dog-toc-sumario"><a href="...">Title text...</a></li>`
	reDogItem = regexp.MustCompile(`(?s)<li class="dog-toc-sumario"><a href="([^"]+)"[^>]*>(.*?)</a></li>`)
	// Captures the pdf url that comes right after each item
	reDogPDF = regexp.MustCompile(`(?s)<p class="dog-descargar"><a href="([^"]+\.pdf)"`)
)

// FetchDOGSumario downloads all 5 section pages for a single day and emits
// items. Falls back to silent empty result when the day has no publication.
func FetchDOGSumario(ctx context.Context, client *http.Client, year, dateYYYYMMDD string) ([]Item, error) {
	isoDate := dateYYYYMMDD[0:4] + "-" + dateYYYYMMDD[4:6] + "-" + dateYYYYMMDD[6:8]
	var out []Item
	anyOk := false
	for n := 1; n <= 5; n++ {
		items, ok, err := fetchDOGSection(ctx, client, year, dateYYYYMMDD, n)
		if err != nil {
			return nil, err
		}
		if ok {
			anyOk = true
		}
		for _, it := range items {
			it.Date = isoDate
			out = append(out, it)
		}
	}
	if !anyOk {
		// 404 in all sections: empty publication or wrong date.
		return nil, nil
	}
	return out, nil
}

func fetchDOGSection(ctx context.Context, client *http.Client, year, dateYYYYMMDD string, section int) ([]Item, bool, error) {
	url := fmt.Sprintf(dogSectionURL, year, dateYYYYMMDD, section)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "knowledge-cli/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("dog http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, false, nil // no such section on that day
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, false, fmt.Errorf("dog http %d: %s", resp.StatusCode, string(b))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, err
	}
	html := string(body)
	return parseDOGSectionHTML(html, section), true, nil
}

// parseDOGSectionHTML walks the section HTML linearly and emits one Item per
// `<li class="dog-toc-sumario">` block, attributing the most recently seen
// section heading and organism to it. The PDF link, if present immediately
// after the item, is attached.
func parseDOGSectionHTML(html string, section int) []Item {
	// We walk the string by index, capturing the latest seen section
	// heading and organism, and emit an item every time we encounter the
	// next dog-toc-sumario block. The PDF link is found by scanning a
	// short forward window from the end of the <li>.
	var (
		currentSectionName = sectionDisplay(section)
		currentSectionCode = sectionCodeStr(section)
		currentOrg         string
		items              []Item
	)
	// Build a single timeline of (pos, kind, payload) using each regex
	// and sort. Cheap enough for typical section sizes (kB).
	type ev struct {
		pos     int
		kind    string // section / org / item
		matches []string
	}
	var events []ev

	for _, m := range reDogSection.FindAllStringSubmatchIndex(html, -1) {
		txt := html[m[2]:m[3]]
		events = append(events, ev{pos: m[0], kind: "section", matches: []string{txt}})
	}
	for _, m := range reDogOrg.FindAllStringSubmatchIndex(html, -1) {
		txt := html[m[2]:m[3]]
		events = append(events, ev{pos: m[0], kind: "org", matches: []string{txt}})
	}
	for _, m := range reDogItem.FindAllStringSubmatchIndex(html, -1) {
		href := html[m[2]:m[3]]
		body := html[m[4]:m[5]]
		// The pdf link sits within ~400 bytes after the closing </li>; scan.
		end := m[1]
		searchEnd := end + 600
		if searchEnd > len(html) {
			searchEnd = len(html)
		}
		pdf := ""
		if loc := reDogPDF.FindStringSubmatchIndex(html[end:searchEnd]); loc != nil {
			pdf = html[end+loc[2] : end+loc[3]]
		}
		events = append(events, ev{pos: m[0], kind: "item", matches: []string{href, body, pdf}})
	}
	// Sort by pos (stable).
	for i := 1; i < len(events); i++ {
		for j := i; j > 0 && events[j].pos < events[j-1].pos; j-- {
			events[j], events[j-1] = events[j-1], events[j]
		}
	}

	for _, e := range events {
		switch e.kind {
		case "section":
			currentSectionName = NormalizeWhitespace(stripTags(e.matches[0]))
			if code := extractSectionCode(currentSectionName); code != "" {
				currentSectionCode = code
			}
		case "org":
			currentOrg = NormalizeWhitespace(stripTags(e.matches[0]))
		case "item":
			href := e.matches[0]
			body := NormalizeWhitespace(stripTags(e.matches[1]))
			pdf := e.matches[2]
			id := dogIDFromHref(href)
			it := Item{
				Source:      "DOG",
				ID:          id,
				SectionCode: currentSectionCode,
				SectionName: currentSectionName,
				Department:  currentOrg,
				Title:       body,
				URLHTML:     absoluteDogURL(href),
				URLPDF:      absoluteDogURL(pdf),
				Score:       -1,
			}
			items = append(items, it)
		}
	}
	return items
}

func absoluteDogURL(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "http") {
		return href
	}
	return "https://www.xunta.gal" + href
}

func dogIDFromHref(href string) string {
	// /dog/Publicados/2025/20250502/AnuncioG0762-210425-0004_es.html
	parts := strings.Split(href, "/")
	if len(parts) == 0 {
		return ""
	}
	last := parts[len(parts)-1]
	last = strings.TrimSuffix(last, "_es.html")
	last = strings.TrimSuffix(last, "_gl.html")
	return "DOG-" + last
}

func stripTags(s string) string {
	// Very small stripper; the DOG sumario only contains short anchors/text.
	out := make([]byte, 0, len(s))
	in := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '<':
			in = true
		case c == '>':
			in = false
		case !in:
			out = append(out, c)
		}
	}
	return string(out)
}

func sectionDisplay(n int) string {
	switch n {
	case 1:
		return "I-III. Disposiciones / Otras disposiciones"
	case 2:
		return "II/IV. Autoridades / Oposiciones"
	case 3:
		return "V. Administración de justicia"
	case 4:
		return "VI. Anuncios"
	case 5:
		return "Otras secciones"
	}
	return ""
}

// sectionCodeStr is a coarse mapping from the DOG page number (Secciones1..5)
// to the section roman numeral used by the relevance filter for BOE. The DOG
// puts both the general provisions ("disposiciones generales") and the other
// provisions ("otras disposiciones") on `Secciones1` (the page carries the
// real section names in its `dog-toc-nivel-1` headings).
func sectionCodeStr(page int) string {
	// Default: page 1 contains sections I, II and III; we return "3" so items
	// are not excluded by default when crossed with the BOE filter.
	// extractSectionCode refines it once the real text ("III. Otras...") appears.
	if page == 1 {
		return "3"
	}
	return ""
}

var reSecHeading = regexp.MustCompile(`^([IVX]+)\.`)

func extractSectionCode(text string) string {
	m := reSecHeading.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return ""
	}
	return romanToCode(m[1])
}

func romanToCode(r string) string {
	switch r {
	case "I":
		return "1"
	case "II":
		return "2"
	case "III":
		return "3"
	case "IV":
		return "4"
	case "V":
		return "5"
	case "VI":
		return "6"
	}
	return ""
}
