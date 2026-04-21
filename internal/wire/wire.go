package wire

const (
	RPCProtocol  = "/mediatrix/rpc/1.0.0"
	FileProtocol = "/mediatrix/file/1.0.0"
)

type RPCRequest struct {
	Service string `json:"service"`
	Method  string `json:"method"`
	Params  string `json:"params"`
}

type RPCResponse struct {
	OK     bool   `json:"ok"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type FileRequest struct {
	Key string `json:"key"`
}

type FileHeader struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}
