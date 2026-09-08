package views

type Flash struct {
	Kind    string
	Message string
}

type Page struct {
	Title         string
	Section       string
	CSRFField     any
	RequestID     string
	Authenticated bool
	Flash         *Flash
	ErrorSummary  string
	FieldErrors   map[string]string
	Values        map[string]string
	Data          any
	Timezone      string
	Version       int64
	Capacity      string
}
