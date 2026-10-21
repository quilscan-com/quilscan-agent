package qclient

// ManageTUI forwards qclient's display contract without reformatting its text.
type ManageTUI struct {
	SchemaVersion int           `json:"schema_version"`
	Compact       ManageTUIView `json:"compact"`
	Expanded      ManageTUIView `json:"expanded"`
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
	Compact  ManageRowTUIView `json:"compact"`
	Expanded ManageRowTUIView `json:"expanded"`
}

type ManageRowTUIView struct {
	Cells  map[string]string `json:"cells"`
	Detail string            `json:"detail"`
	Tone   string            `json:"tone"`
	Tones  map[string]string `json:"tones"`
}
