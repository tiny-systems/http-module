package etc

import "github.com/swaggest/jsonschema-go"

const (
	HeaderContentType   = "Content-Type"
	MIMEApplicationJSON = "application/json"
	MIMEApplicationXML  = "application/xml"
	MIMETextXML         = "text/xml"
	MimeTextPlain       = "text/plain"
	MIMETextHTML        = "text/html"
	MIMEApplicationForm = "application/x-www-form-urlencoded"
	MIMEMultipartForm   = "multipart/form-data"
)

type ContentType string

func (c ContentType) JSONSchema() (jsonschema.Schema, error) {
	contentType := jsonschema.Schema{}
	contentType.AddType(jsonschema.String)
	contentType.WithTitle("Content Type").
		WithDefault(200).
		WithEnum(MIMEApplicationJSON, MIMEApplicationXML, MIMETextHTML, MimeTextPlain).
		WithDefault(MIMEApplicationJSON).
		WithDescription("Content type of the response")
	return contentType, nil
}
