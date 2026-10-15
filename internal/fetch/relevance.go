package fetch

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Item is a normalised disposition discovered in a daily sumario. Fields are
// populated by the BOE / DOG parsers before relevance filtering and download.
type Item struct {
	Source      string // "BOE" or "DOG"
	ID          string // BOE-A-2024-10235 / DOG-2024-AnuncioG0762-210425-0004
	Date        string // YYYY-MM-DD of publication
	SectionCode string // "1", "3", ...
	SectionName string // "I. Disposiciones generales", "III. Otras disposiciones"
	Department  string // raw name from sumario
	Epigraph    string // sub-rubric ("Medidas urgentes", ...)
	Title       string // full title text
	URLHTML     string
	URLPDF      string
	URLXML      string

	// Filled by relevance filtering.
	MatchedAxis string  // "norm", "department", "keyword", or "" if the coarse filter did not match
	Score       float32 // max cosine vs queries-of-relevance set; -1 if not computed
}

// --- Axis A: modified-statute regexes -------------------------------------

var (
	reModifica = regexp.MustCompile(`(?i)\bmodifica(?:ci[oó]n)?\b`)

	normPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)Ley de Sociedades de Capital|\bLSC\b|Real Decreto Legislativo 1/2010`),
		regexp.MustCompile(`(?i)Reglamento del Registro Mercantil|\bRRM\b|Real Decreto 1784/1996`),
		regexp.MustCompile(`(?i)\bLIRPF\b|Ley del IRPF|Ley 35/2006`),
		regexp.MustCompile(`(?i)\bLIS\b|Ley del Impuesto sobre Sociedades|Ley 27/2014`),
		regexp.MustCompile(`(?i)\bLGT\b|Ley General Tributaria|Ley 58/2003`),
		regexp.MustCompile(`(?i)\bIVA\b|Ley 37/1992`),
		regexp.MustCompile(`(?i)\bLCSP\b|Ley 9/2017`),
		regexp.MustCompile(`(?i)C[oó]digo de Comercio`),
		regexp.MustCompile(`(?i)Estatuto de los Trabajadores|Real Decreto Legislativo 2/2015`),
		regexp.MustCompile(`(?i)\bLGSS\b|Real Decreto Legislativo 8/2015`),
		regexp.MustCompile(`(?i)Ley Concursal|Real Decreto Legislativo 1/2020`),
		regexp.MustCompile(`(?i)Modificaciones Estructurales|RDLeg 5/2023|Real Decreto-ley 5/2023`),
		regexp.MustCompile(`(?i)\bRGPD\b|\bLOPDGDD\b|Ley Org[aá]nica 3/2018`),
		regexp.MustCompile(`(?i)Crea y Crece|Ley 18/2022`),
		regexp.MustCompile(`(?i)Auditor[ií]a de Cuentas|Ley 22/2015`),
	}
)

// --- Axis B: allowed departments and sections ----------------------------

var departmentAllowed = []*regexp.Regexp{
	regexp.MustCompile(`(?i)MINISTERIO DE JUSTICIA`),
	regexp.MustCompile(`(?i)MINISTERIO DE HACIENDA`),
	regexp.MustCompile(`(?i)MINISTERIO DE TRABAJO Y ECONOM[IÍ]A SOCIAL`),
	regexp.MustCompile(`(?i)MINISTERIO DE INDUSTRIA`),
	regexp.MustCompile(`(?i)MINISTERIO DE ASUNTOS ECON[OÓ]MICOS`),
	regexp.MustCompile(`(?i)MINISTERIO DE LA PRESIDENCIA, JUSTICIA`),
}

// alwaysOnDepartments match regardless of section.
var departmentAlways = []*regexp.Regexp{
	regexp.MustCompile(`(?i)DIRECCI[OÓ]N GENERAL DE SEGURIDAD JUR[IÍ]DICA Y FE P[UÚ]BLICA`),
	regexp.MustCompile(`(?i)\bDGSJFP\b`),
	regexp.MustCompile(`(?i)AGENCIA ESTATAL DE ADMINISTRACI[OÓ]N TRIBUTARIA`),
	regexp.MustCompile(`(?i)\bAEAT\b`),
}

// --- Axis C: title keywords ----------------------------------------------

var (
	// "sociedad" but not "sociedad civil" or "sociedad cooperativa".
	reSociedad     = regexp.MustCompile(`(?i)\bsociedad(?:es)?\b`)
	reSocCivilCoop = regexp.MustCompile(`(?i)\bsociedad(?:es)?\s+(civil|cooperativa)`)

	keywordPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bmercantil(?:es)?\b`),
		regexp.MustCompile(`(?i)\btributari[oa]s?\b`),
		regexp.MustCompile(`(?i)\bconcursal(?:es)?\b`),
		regexp.MustCompile(`(?i)\bcripto(?:activo)?s?\b`),
		regexp.MustCompile(`\bIRPF\b`),
		regexp.MustCompile(`\bIS\b`),
		regexp.MustCompile(`\bIVA\b`),
		regexp.MustCompile(`\bIAE\b`),
		regexp.MustCompile(`(?i)\baut[oó]nom[oa]s?\b`),
		regexp.MustCompile(`(?i)\bRETA\b`),
		regexp.MustCompile(`(?i)protecci[oó]n de datos`),
		regexp.MustCompile(`(?i)\bRGPD\b`),
		regexp.MustCompile(`(?i)\bLOPDGDD\b`),
		regexp.MustCompile(`(?i)\bconsumidor(?:es)?\b`),
		regexp.MustCompile(`(?i)contrato del sector p[uú]blico|licitaci[oó]n`),
		regexp.MustCompile(`(?i)registro mercantil`),
		regexp.MustCompile(`(?i)\bsubvenci[oó]n(?:es)?\b`),
	}
)

// CoarseFilter applies axes A (modified statute), B (department) and C
// (keyword) over an item's title/department/section. Returns axis name that
// matched ("norm" / "department" / "keyword"), or "" when none does.
func CoarseFilter(it Item) string {
	title := it.Title

	// Axis A: "modifica..." + statute
	if reModifica.MatchString(title) {
		for _, r := range normPatterns {
			if r.MatchString(title) {
				return "norm"
			}
		}
	}

	// Axis B: always-relevant departments (DGSJFP, AEAT).
	for _, r := range departmentAlways {
		if r.MatchString(it.Department) {
			return "department"
		}
	}

	// Axis B: allowed ministries only in section I or III.
	if it.SectionCode == "1" || it.SectionCode == "3" {
		for _, r := range departmentAllowed {
			if r.MatchString(it.Department) {
				return "department"
			}
		}
	}

	// Axis C: title keywords.
	// Special case: exclude "sociedad civil" and "sociedad cooperativa".
	if reSociedad.MatchString(title) && !reSocCivilCoop.MatchString(title) {
		return "keyword"
	}
	for _, r := range keywordPatterns {
		if r.MatchString(title) {
			return "keyword"
		}
	}
	return ""
}

// DefaultRelevanceQueries are generic example seeds, used only when the user has
// no queries file. Each seed is a natural-language query whose embedding is
// compared (max cosine) against an item's title. Write your own, one per line,
// in RelevanceQueriesPath(); keep them in Spanish so they land in the same
// region of the embedding space as BOE/DOG titles.
var DefaultRelevanceQueries = []string{
	"modificación de la ley de sociedades de capital",
	"impuesto sobre sociedades para pymes",
	"ley general tributaria y procedimientos",
	"protección de datos personales",
}

// RelevanceQueriesPath is $KNOWLEDGE_RELEVANCE_QUERIES, or
// ~/.config/knowledge/relevance-queries.txt.
func RelevanceQueriesPath() string {
	if p := os.Getenv("KNOWLEDGE_RELEVANCE_QUERIES"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "knowledge", "relevance-queries.txt")
}

// LoadRelevanceQueries reads one query per line, skipping blank lines and
// lines starting with '#'. A missing file yields DefaultRelevanceQueries; a
// file with no queries is an error.
func LoadRelevanceQueries(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return DefaultRelevanceQueries, nil
	}
	if err != nil {
		return nil, err
	}
	var queries []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		queries = append(queries, line)
	}
	if len(queries) == 0 {
		return nil, fmt.Errorf("%s has no relevance queries", path)
	}
	return queries, nil
}

// Cosine returns the cosine similarity between two equally-sized float32 vectors.
func Cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float32
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (sqrt32(na) * sqrt32(nb))
}

func sqrt32(x float32) float32 {
	if x <= 0 {
		return 0
	}
	return float32(math.Sqrt(float64(x)))
}

// MaxCosine returns the maximum cosine similarity between vec and any vector
// in refs. Returns -1 if refs is empty (i.e. no semantic ranking possible).
func MaxCosine(vec []float32, refs [][]float32) (float32, int) {
	if len(refs) == 0 {
		return -1, -1
	}
	best := float32(-2)
	bestIdx := -1
	for i, r := range refs {
		c := Cosine(vec, r)
		if c > best {
			best = c
			bestIdx = i
		}
	}
	return best, bestIdx
}

// NormalizeWhitespace collapses internal whitespace runs and trims.
func NormalizeWhitespace(s string) string {
	fields := strings.Fields(s)
	return strings.Join(fields, " ")
}
