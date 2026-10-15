package fetch

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"time"
)

// BOE open-data endpoint. The XML payload is wrapped in a <response> with
// the actual sumario inside <data><sumario>...</sumario></data>.
const boeSumarioURL = "https://www.boe.es/datosabiertos/api/boe/sumario/%s" // YYYYMMDD

// boeResponse mirrors the XML wrapper.
type boeResponse struct {
	XMLName xml.Name `xml:"response"`
	Status  struct {
		Code int    `xml:"code"`
		Text string `xml:"text"`
	} `xml:"status"`
	Data struct {
		Sumario boeSumario `xml:"sumario"`
	} `xml:"data"`
}

type boeSumario struct {
	Metadatos struct {
		Publicacion      string `xml:"publicacion"`
		FechaPublicacion string `xml:"fecha_publicacion"` // YYYYMMDD
	} `xml:"metadatos"`
	Diario boeDiario `xml:"diario"`
}

type boeDiario struct {
	Numero    string       `xml:"numero,attr"`
	Secciones []boeSeccion `xml:"seccion"`
}

type boeSeccion struct {
	Codigo        string            `xml:"codigo,attr"`
	Nombre        string            `xml:"nombre,attr"`
	Departamentos []boeDepartamento `xml:"departamento"`
}

type boeDepartamento struct {
	Codigo    string        `xml:"codigo,attr"`
	Nombre    string        `xml:"nombre,attr"`
	Epigrafes []boeEpigrafe `xml:"epigrafe"`
	Items     []boeItem     `xml:"item"` // items may also hang directly off the department
}

type boeEpigrafe struct {
	Nombre string    `xml:"nombre,attr"`
	Items  []boeItem `xml:"item"`
}

type boeItem struct {
	Identificador string `xml:"identificador"`
	Control       string `xml:"control"`
	Titulo        string `xml:"titulo"`
	URLPDF        string `xml:"url_pdf"`
	URLHTML       string `xml:"url_html"`
	URLXML        string `xml:"url_xml"`
}

// FetchBOESumario downloads the BOE summary for a YYYYMMDD date and returns
// the discovered items already normalised into the fetch.Item shape. The
// caller decides what to do with them (filter, rank, download).
func FetchBOESumario(ctx context.Context, client *http.Client, dateYYYYMMDD string) ([]Item, error) {
	url := fmt.Sprintf(boeSumarioURL, dateYYYYMMDD)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/xml")
	req.Header.Set("User-Agent", "knowledge-cli/0.1")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("boe http: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		// No publication that day (weekend or holiday): empty, not an error.
		return nil, nil
	}
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("boe http %d: %s", resp.StatusCode, string(b))
	}

	var br boeResponse
	dec := xml.NewDecoder(resp.Body)
	if err := dec.Decode(&br); err != nil {
		return nil, fmt.Errorf("boe xml decode: %w", err)
	}

	// status.code 404 inside the XML wrapper also means "no sumario".
	if br.Status.Code == 404 {
		return nil, nil
	}
	if br.Status.Code != 200 {
		return nil, fmt.Errorf("boe api status %d: %s", br.Status.Code, br.Status.Text)
	}

	pubDate, err := parseYYYYMMDD(br.Data.Sumario.Metadatos.FechaPublicacion)
	if err != nil {
		pubDate = dateYYYYMMDD // fallback
	}
	isoDate := pubDate[0:4] + "-" + pubDate[4:6] + "-" + pubDate[6:8]

	var out []Item
	for _, sec := range br.Data.Sumario.Diario.Secciones {
		for _, dep := range sec.Departamentos {
			// Items hanging directly off the department.
			for _, it := range dep.Items {
				out = append(out, makeBOEItem(it, sec, dep, "", isoDate))
			}
			for _, ep := range dep.Epigrafes {
				for _, it := range ep.Items {
					out = append(out, makeBOEItem(it, sec, dep, ep.Nombre, isoDate))
				}
			}
		}
	}
	return out, nil
}

func makeBOEItem(it boeItem, sec boeSeccion, dep boeDepartamento, epigraph, isoDate string) Item {
	return Item{
		Source:      "BOE",
		ID:          it.Identificador,
		Date:        isoDate,
		SectionCode: sec.Codigo,
		SectionName: sec.Nombre,
		Department:  dep.Nombre,
		Epigraph:    epigraph,
		Title:       NormalizeWhitespace(it.Titulo),
		URLHTML:     it.URLHTML,
		URLPDF:      it.URLPDF,
		URLXML:      it.URLXML,
		Score:       -1,
	}
}

func parseYYYYMMDD(s string) (string, error) {
	if len(s) != 8 {
		return "", fmt.Errorf("not YYYYMMDD: %q", s)
	}
	if _, err := time.Parse("20060102", s); err != nil {
		return "", err
	}
	return s, nil
}
