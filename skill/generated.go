package skill

type GeneratedResult struct {
	Definition *Definition
	Rejection  *Rejection
}

type Rejection struct {
	Schema string         `json:"schema"`
	Error  RejectionError `json:"error"`
}

type RejectionError struct {
	Code        string   `json:"code"`
	Message     string   `json:"message"`
	Unsupported []string `json:"unsupported"`
}
