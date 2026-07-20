package main

import "encoding/xml"

type xmlStatus struct {
	XMLName xml.Name  `xml:"status"`
	Name    string    `xml:"name,attr"`
	Items   []xmlItem `xml:"item"`
}

type xmlItem struct {
	Key   string `xml:"key,attr"`
	Value int    `xml:",chardata"`
}

func main() {
	status := xmlStatus{
		Name: "cg12",
		Items: []xmlItem{
			{Key: "alpha", Value: 17},
			{Key: "beta", Value: 25},
		},
	}

	encoded, err := xml.Marshal(status)
	if err != nil {
		panic("xml Marshal failed")
	}

	var decoded xmlStatus
	if err := xml.Unmarshal(encoded, &decoded); err != nil {
		panic("xml Unmarshal failed")
	}
	if decoded.Name != "cg12" {
		panic("xml name mismatch")
	}
	if len(decoded.Items) != 2 {
		panic("xml item count mismatch")
	}
	if decoded.Items[0].Key != "alpha" || decoded.Items[0].Value != 17 {
		panic("xml first item mismatch")
	}
	if decoded.Items[1].Key != "beta" || decoded.Items[1].Value != 25 {
		panic("xml second item mismatch")
	}
}
