package qclient

// ManageTUI retains compact display metadata without reformatting its text.
// Expanded presentation is ignored on input and omitted from all reports.
type ManageTUI struct {
	SchemaVersion int           `json:"schema_version"`
	Compact       ManageTUIView `json:"compact"`
}

type ManageTUIView struct {
	Header          []string       `json:"header"`
	Allocations     ManageTUITable `json:"allocations"`
	AvailableShards ManageTUITable `json:"available_shards"`
}

type ManageTUITable struct {
	Heading []string          `json:"heading"`
	Columns []ManageTUIColumn `json:"columns"`
}

type ManageTUIColumn struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type ManageRowTUI struct {
	Compact ManageRowTUIView `json:"compact"`
}

type ManageRowTUIView struct {
	Cells  map[string]string `json:"cells"`
	Detail string            `json:"detail"`
	Tone   string            `json:"tone"`
	Tones  map[string]string `json:"tones"`
}
