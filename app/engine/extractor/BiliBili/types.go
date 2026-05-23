package BiliBili

type roomInitResponse struct {
	Code int `json:"code"`
	Data struct {
		RoomID     int  `json:"room_id"`
		LiveStatus int  `json:"live_status"`
		IsLocked   bool `json:"is_locked"`
		Encrypted  bool `json:"encrypted"`
	} `json:"data"`
}

type playInfoResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    struct {
		RoomID      int `json:"room_id"`
		LiveStatus  int `json:"live_status"`
		PlayURLInfo struct {
			PlayURL struct {
				CID     int          `json:"cid"`
				QnDesc  []qnDescItem `json:"g_qn_desc"`
				Streams []streamItem `json:"stream"`
			} `json:"playurl"`
		} `json:"playurl_info"`
	} `json:"data"`
}

type qnDescItem struct {
	Qn   int    `json:"qn"`
	Desc string `json:"desc"`
}

type streamItem struct {
	ProtocolName string       `json:"protocol_name"`
	Formats      []formatItem `json:"format"`
}

type formatItem struct {
	FormatName string      `json:"format_name"`
	Codecs     []codecItem `json:"codec"`
}

type codecItem struct {
	CodecName string    `json:"codec_name"`
	CurrentQn int       `json:"current_qn"`
	AcceptQn  []int     `json:"accept_qn"`
	BaseURL   string    `json:"base_url"`
	URLInfo   []urlItem `json:"url_info"`
}

type urlItem struct {
	Host  string `json:"host"`
	Extra string `json:"extra"`
}

type playURLV1Response struct {
	Code int `json:"code"`
	Data struct {
		CurrentQn int `json:"current_qn"`
		Durl      []struct {
			URL   string `json:"url"`
			Order int    `json:"order"`
		} `json:"durl"`
	} `json:"data"`
}
