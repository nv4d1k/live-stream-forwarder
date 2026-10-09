package dash

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

// Representation is one DASH Representation (a single itag for YouTube).
type Representation struct {
	ID        string
	Bandwidth int
	Width     int
	Height    int
	Codecs    string
	BaseURL   string
	// FirstSQ is the MPD SegmentList startNumber (the DVR window start).
	FirstSQ int
	// LastSQ is the sequence number of the last SegmentURL listed in the
	// MPD for this representation (the live edge at MPD fetch time).
	LastSQ int
}

// MPDInfo groups the representations of a parsed MPD by media type.
type MPDInfo struct {
	Audio []Representation
	Video []Representation
}

type mpdXML struct {
	Periods []struct {
		AdaptationSets []struct {
			MimeType string `xml:"mimeType,attr"`
			Reps     []struct {
				ID          string `xml:"id,attr"`
				Bandwidth   int    `xml:"bandwidth,attr"`
				Width       int    `xml:"width,attr"`
				Height      int    `xml:"height,attr"`
				Codecs      string `xml:"codecs,attr"`
				BaseURL     string `xml:"BaseURL"`
				SegmentList struct {
					StartNumber int `xml:"startNumber,attr"`
					SegmentURLs []struct {
						Media string `xml:"media,attr"`
					} `xml:"SegmentURL"`
				} `xml:"SegmentList"`
			} `xml:"Representation"`
		} `xml:"AdaptationSet"`
	} `xml:"Period"`
}

// parseMPD extracts the audio and video representations from a YouTube
// live DASH manifest.
func parseMPD(data []byte) (*MPDInfo, error) {
	var doc mpdXML
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse MPD xml: %w", err)
	}
	info := &MPDInfo{}
	for _, p := range doc.Periods {
		for _, as := range p.AdaptationSets {
			for _, r := range as.Reps {
				rep := Representation{
					ID:        r.ID,
					Bandwidth: r.Bandwidth,
					Width:     r.Width,
					Height:    r.Height,
					Codecs:    r.Codecs,
					BaseURL:   r.BaseURL,
					FirstSQ:   r.SegmentList.StartNumber,
				}
				for _, u := range slices.Backward(r.SegmentList.SegmentURLs) {
					if sq, ok := sqFromMediaURL(u.Media); ok {
						rep.LastSQ = sq
						break
					}
				}
				switch {
				case strings.HasPrefix(as.MimeType, "audio/"):
					info.Audio = append(info.Audio, rep)
				case strings.HasPrefix(as.MimeType, "video/"):
					info.Video = append(info.Video, rep)
				}
			}
		}
	}
	if len(info.Audio) == 0 && len(info.Video) == 0 {
		return nil, fmt.Errorf("MPD has no audio or video representations")
	}
	return info, nil
}

// sqFromMediaURL extracts the sequence number from a SegmentURL media
// value like "sq/7990/lmt/1".
func sqFromMediaURL(media string) (int, bool) {
	rest, ok := strings.CutPrefix(media, "sq/")
	if !ok {
		return 0, false
	}
	num, _, _ := strings.Cut(rest, "/")
	n, err := strconv.Atoi(num)
	if err != nil {
		return 0, false
	}
	return n, true
}

// pickRepresentations selects the audio and video representations to
// forward: the highest bandwidth of each set.
func pickRepresentations(info *MPDInfo) (audio, video *Representation, err error) {
	if len(info.Audio) == 0 || len(info.Video) == 0 {
		return nil, nil, fmt.Errorf("MPD lacks audio (%d) or video (%d) representations", len(info.Audio), len(info.Video))
	}
	pick := func(reps []Representation) *Representation {
		best := &reps[0]
		for i := range reps[1:] {
			if reps[i+1].Bandwidth > best.Bandwidth {
				best = &reps[i+1]
			}
		}
		return best
	}
	return pick(info.Audio), pick(info.Video), nil
}

// fetchAndParseMPD downloads and parses the MPD at mpdURL.
func fetchAndParseMPD(hc *http.Client, mpdURL string, headers http.Header) (*MPDInfo, error) {
	req, err := http.NewRequest(http.MethodGet, mpdURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create MPD request: %w", err)
	}
	for k := range headers {
		req.Header.Set(k, headers.Get(k))
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request MPD: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MPD fetch got status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read MPD: %w", err)
	}
	return parseMPD(data)
}
