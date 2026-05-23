package DouYu

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"

	"github.com/iceking2nd/go-toolkits/converter"
	"github.com/nv4d1k/live-stream-forwarder/global"
)

type streamParameters struct {
	RoomID    uint64 `json:"roomID"`
	Nonce     string `json:"nonce"`
	OwnerUID  uint64 `json:"owner_uid"`
	CookiePre string `json:"cookiePre"`
}

func (l *Link) getLegacyFirstStreamParameters(rfid string) (sp streamParameters, err error) {
	log := global.Log.WithField("func", "app.engine.extractor.DouYu.getLegacyFirstStreamParameters")
	log.WithField("rfid", rfid).Debugln("fetching stream parameters")
	streamParamRegex := regexp.MustCompile(`window.preloadStreamUrlPromise = getLegacyFirstStream\((.*)\)\s?;`)
	resp, err := l.client.Get(fmt.Sprintf("https://www.douyu.com/%s", rfid))
	if err != nil {
		log.WithError(err).Errorln("failed to request room page")
		return sp, fmt.Errorf("send request error when getting real room id: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.WithError(err).Errorln("failed to read room page response body")
		return sp, fmt.Errorf("parsing response body error when getting real room id: %w", err)
	}
	l.res = string(body)
	streamParam := streamParamRegex.FindStringSubmatch(l.res)
	if len(streamParam) != 2 {
		log.Warnln("stream parameters not found in room page")
		return sp, errors.New("invalidate stream parameters")
	}
	spjson, err := converter.JSObj2JSON(streamParam[1])
	if err != nil {
		log.WithError(err).Errorln("failed to convert stream parameters JS object to JSON")
		return sp, fmt.Errorf("parsing stream parameters error: %w", err)
	}
	err = json.Unmarshal(spjson, &sp)
	if err != nil {
		log.WithError(err).Errorln("failed to unmarshal stream parameters JSON")
		return sp, err
	}
	log.WithField("roomID", sp.RoomID).Infoln("stream parameters resolved")
	return sp, nil
}
