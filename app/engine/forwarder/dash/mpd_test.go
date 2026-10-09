package dash

import (
	"fmt"
	"testing"
)

const testMPDTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<MPD xmlns="urn:mpeg:DASH:schema:MPD:2011" type="dynamic" minimumUpdatePeriod="PT2.000S">
 <Period start="PT0.000S">
  <AdaptationSet id="0" mimeType="audio/mp4" subsegmentAlignment="true">
   <Representation id="139" codecs="mp4a.40.5" audioSamplingRate="22050" bandwidth="64000">
    <BaseURL>%s/audio139/</BaseURL>
    <SegmentList startNumber="100" timescale="1000">
     <SegmentURL media="sq/100/lmt/1"/>
     <SegmentURL media="sq/101/lmt/1"/>
     <SegmentURL media="sq/102/lmt/1"/>
    </SegmentList>
   </Representation>
   <Representation id="140" codecs="mp4a.40.2" audioSamplingRate="44100" bandwidth="144000">
    <BaseURL>%s/audio140/</BaseURL>
    <SegmentList startNumber="100" timescale="1000">
     <SegmentURL media="sq/100/lmt/1"/>
     <SegmentURL media="sq/101/lmt/1"/>
     <SegmentURL media="sq/102/lmt/1"/>
    </SegmentList>
   </Representation>
  </AdaptationSet>
  <AdaptationSet id="1" mimeType="video/mp4" subsegmentAlignment="true">
   <Representation id="160" codecs="avc1.4d400c" width="256" height="144" bandwidth="192223" frameRate="30">
    <BaseURL>%s/video160/</BaseURL>
    <SegmentList startNumber="100" timescale="1000">
     <SegmentURL media="sq/100/lmt/1"/>
     <SegmentURL media="sq/101/lmt/1"/>
     <SegmentURL media="sq/102/lmt/1"/>
    </SegmentList>
   </Representation>
   <Representation id="299" codecs="avc1.4d402a" width="1920" height="1080" bandwidth="5144200" frameRate="60">
    <BaseURL>%s/video299/</BaseURL>
    <SegmentList startNumber="100" timescale="1000">
     <SegmentURL media="sq/100/lmt/1"/>
     <SegmentURL media="sq/101/lmt/1"/>
     <SegmentURL media="sq/102/lmt/1"/>
    </SegmentList>
   </Representation>
  </AdaptationSet>
 </Period>
</MPD>
`

func testMPD(base string) []byte {
	return fmt.Appendf(nil, testMPDTemplate, base, base, base, base)
}

func TestSqFromMediaURL(t *testing.T) {
	tests := []struct {
		in     string
		want   int
		wantOK bool
	}{
		{"sq/7990/lmt/1", 7990, true},
		{"sq/0/lmt/1", 0, true},
		{"sq/123", 123, true},
		{"lmt/1", 0, false},
		{"sq/abc/lmt/1", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := sqFromMediaURL(tc.in)
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("sqFromMediaURL(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.wantOK)
		}
	}
}

func TestParseMPD(t *testing.T) {
	info, err := parseMPD(testMPD("http://example.com"))
	if err != nil {
		t.Fatalf("parseMPD error: %v", err)
	}
	if len(info.Audio) != 2 {
		t.Fatalf("audio representations = %d, want 2", len(info.Audio))
	}
	if len(info.Video) != 2 {
		t.Fatalf("video representations = %d, want 2", len(info.Video))
	}
	for _, rep := range info.Audio {
		if rep.LastSQ != 102 {
			t.Errorf("audio rep %s LastSQ = %d, want 102 (last SegmentURL)", rep.ID, rep.LastSQ)
		}
	}
	for _, rep := range info.Video {
		if rep.LastSQ != 102 {
			t.Errorf("video rep %s LastSQ = %d, want 102", rep.ID, rep.LastSQ)
		}
	}
	// Spot-check attributes of the highest video rep.
	var best *Representation
	for i := range info.Video {
		if best == nil || info.Video[i].Bandwidth > best.Bandwidth {
			best = &info.Video[i]
		}
	}
	if best.ID != "299" || best.Width != 1920 || best.Height != 1080 {
		t.Errorf("best video rep = %+v, want itag 299 1920x1080", best)
	}
	if best.BaseURL != "http://example.com/video299/" {
		t.Errorf("best video BaseURL = %q", best.BaseURL)
	}
}

func TestParseMPD_InvalidXML(t *testing.T) {
	if _, err := parseMPD([]byte("not xml at all")); err == nil {
		t.Error("parseMPD should fail on invalid XML")
	}
}

func TestPickRepresentations(t *testing.T) {
	info, err := parseMPD(testMPD("http://example.com"))
	if err != nil {
		t.Fatalf("parseMPD error: %v", err)
	}
	audio, video, err := pickRepresentations(info)
	if err != nil {
		t.Fatalf("pickRepresentations error: %v", err)
	}
	if audio.ID != "140" {
		t.Errorf("audio rep = %s, want 140 (highest bandwidth)", audio.ID)
	}
	if video.ID != "299" {
		t.Errorf("video rep = %s, want 299 (highest bandwidth)", video.ID)
	}
}

func TestPickRepresentations_MissingSet(t *testing.T) {
	if _, _, err := pickRepresentations(&MPDInfo{}); err == nil {
		t.Error("pickRepresentations should fail with no representations")
	}
	if _, _, err := pickRepresentations(&MPDInfo{Audio: []Representation{{ID: "140"}}}); err == nil {
		t.Error("pickRepresentations should fail with no video representation")
	}
}
