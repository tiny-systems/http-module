package etc

type Header struct {
	Key   string `json:"key" required:"true" title:"Key" colSpan:"col-span-6"`
	Value string `json:"value" required:"true" title:"Value" colSpan:"col-span-6"`
}
