package har

// Page is a HAR page object. Mirage reads pages but never writes them.
type Page struct {
	StartedDateTime Timestamp   `json:"startedDateTime"`
	ID              string      `json:"id"`
	Title           string      `json:"title"`
	PageTimings     PageTimings `json:"pageTimings"`
	Comment         string      `json:"comment,omitempty"`
}

// PageTimings holds page load milestones in milliseconds (-1 = unknown).
type PageTimings struct {
	OnContentLoad float64 `json:"onContentLoad"`
	OnLoad        float64 `json:"onLoad"`
	Comment       string  `json:"comment,omitempty"`
}

// Entry is one HAR entry: a request, its response and the timings.
type Entry struct {
	// Pageref links the entry to Page.ID.
	Pageref string `json:"pageref,omitempty"`
	// StartedDateTime is when the request started (UTC when written).
	StartedDateTime Timestamp `json:"startedDateTime"`
	// Time is the total elapsed time in milliseconds (-1 = unknown).
	Time     float64  `json:"time"`
	Request  Request  `json:"request"`
	Response Response `json:"response"`
	Cache    Cache    `json:"cache"`
	Timings  Timings  `json:"timings"`
	// ServerIPAddress and Connection are informational.
	ServerIPAddress string `json:"serverIPAddress,omitempty"`
	Connection      string `json:"connection,omitempty"`
	Comment         string `json:"comment,omitempty"`
}

// Request is the HAR request object.
type Request struct {
	Method      string      `json:"method"`
	URL         string      `json:"url"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []Cookie    `json:"cookies"`
	Headers     []NameValue `json:"headers"`
	QueryString []NameValue `json:"queryString"`
	PostData    *PostData   `json:"postData,omitempty"`
	// HeadersSize and BodySize are in bytes, -1 when unknown.
	HeadersSize Size   `json:"headersSize"`
	BodySize    Size   `json:"bodySize"`
	Comment     string `json:"comment,omitempty"`
}

// Response is the HAR response object.
type Response struct {
	Status      int         `json:"status"`
	StatusText  string      `json:"statusText"`
	HTTPVersion string      `json:"httpVersion"`
	Cookies     []Cookie    `json:"cookies"`
	Headers     []NameValue `json:"headers"`
	Content     Content     `json:"content"`
	RedirectURL string      `json:"redirectURL"`
	HeadersSize Size        `json:"headersSize"`
	BodySize    Size        `json:"bodySize"`
	Comment     string      `json:"comment,omitempty"`
	// Error is Chrome's "_error" extension: the reason a request failed.
	// Mirage also uses it to carry Exchange.Error.
	Error string `json:"_error,omitempty"`
}

// NameValue is a header, query string parameter or similar pair.
type NameValue struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Comment string `json:"comment,omitempty"`
}

// Cookie is a request or response cookie.
type Cookie struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Path    string `json:"path,omitempty"`
	Domain  string `json:"domain,omitempty"`
	Expires string `json:"expires,omitempty"`
	// HTTPOnly and Secure are omitted when false.
	HTTPOnly bool   `json:"httpOnly,omitempty"`
	Secure   bool   `json:"secure,omitempty"`
	Comment  string `json:"comment,omitempty"`
}

// PostData describes a request body.
type PostData struct {
	MimeType string  `json:"mimeType"`
	Params   []Param `json:"params,omitempty"`
	Text     string  `json:"text,omitempty"`
	// Encoding is "base64" when Text carries a binary body. HAR 1.2 has no
	// such field for postData (only for response content); Mirage mirrors the
	// content object so non-UTF-8 request bodies survive an export.
	Encoding string `json:"encoding,omitempty"`
	// Truncated is Mirage's "_mirageTruncated" extension: the body was cut
	// short when it was captured.
	Truncated bool   `json:"_mirageTruncated,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// Param is one posted form field.
type Param struct {
	Name        string `json:"name"`
	Value       string `json:"value,omitempty"`
	FileName    string `json:"fileName,omitempty"`
	ContentType string `json:"contentType,omitempty"`
	Comment     string `json:"comment,omitempty"`
}

// Content is the response body description.
type Content struct {
	// Size is the length of the (decoded) body in bytes.
	Size Size `json:"size"`
	// Compression is the number of bytes saved by compression, if known.
	Compression Size   `json:"compression,omitempty"`
	MimeType    string `json:"mimeType"`
	Text        string `json:"text,omitempty"`
	// Encoding is "base64" when Text holds a base64 encoded binary body.
	Encoding string `json:"encoding,omitempty"`
	// Truncated is Mirage's "_mirageTruncated" extension: the body was cut
	// short when it was captured.
	Truncated bool   `json:"_mirageTruncated,omitempty"`
	Comment   string `json:"comment,omitempty"`
}

// Cache is the HAR cache object. Mirage does not interpret it.
type Cache struct {
	BeforeRequest *CacheState `json:"beforeRequest,omitempty"`
	AfterRequest  *CacheState `json:"afterRequest,omitempty"`
	Comment       string      `json:"comment,omitempty"`
}

// CacheState is a cache entry state.
type CacheState struct {
	Expires    string `json:"expires,omitempty"`
	LastAccess string `json:"lastAccess"`
	ETag       string `json:"eTag"`
	HitCount   int    `json:"hitCount"`
	Comment    string `json:"comment,omitempty"`
}

// Timings holds the phases of a request in milliseconds. -1 means the phase
// does not apply (blocked, dns, connect, ssl) and 0 or more is a duration.
// Send, Wait and Receive are required by the specification. SSL time is
// included in Connect.
type Timings struct {
	Blocked float64 `json:"blocked"`
	DNS     float64 `json:"dns"`
	Connect float64 `json:"connect"`
	Send    float64 `json:"send"`
	Wait    float64 `json:"wait"`
	Receive float64 `json:"receive"`
	SSL     float64 `json:"ssl"`
	Comment string  `json:"comment,omitempty"`
}
