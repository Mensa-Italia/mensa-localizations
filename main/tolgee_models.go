package main

type TolgeeModel struct {
	Embedded struct {
		Languages []struct {
			Id           int    `json:"id"`
			Name         string `json:"name"`
			Tag          string `json:"tag"`
			OriginalName string `json:"originalName"`
			FlagEmoji    string `json:"flagEmoji"`
			Base         bool   `json:"base"`
		} `json:"languages"`
	} `json:"_embedded"`
	Links struct {
		Self struct {
			Href string `json:"href"`
		} `json:"self"`
	} `json:"_links"`
	Page struct {
		Size          int `json:"size"`
		TotalElements int `json:"totalElements"`
		TotalPages    int `json:"totalPages"`
		Number        int `json:"number"`
	} `json:"page"`
}
