package HuYa

import (
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"
	"github.com/nv4d1k/live-stream-forwarder/global"
	"github.com/tidwall/gjson"
)

func (l *Link) getAnonymousUID() (err error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.getAnonymousUID")
	var (
		resp *http.Response
		body []byte
	)
	data := `{
        "appId": 5002,
        "byPass": 3,
        "context": "",
        "version": "2.4",
        "data": {}
    }`
	resp, err = l.client.Post("https://udblgn.huya.com/web/anonymousLogin", "application/json", strings.NewReader(data))
	if err != nil {
		return fmt.Errorf("sending get anonymous uid request error: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("parsing get anonymous uid response body error: %w", err)
	}
	log.WithField("field", "anonymous uid response body").Debug(string(body))
	if !gjson.GetBytes(body, "data.uid").Exists() {
		return errors.New("anonymous user id not found")
	}
	l.uid = gjson.GetBytes(body, "data.uid").String()
	l.uidi = gjson.GetBytes(body, "data.uid").Int()
	return nil
}

func (l *Link) getRoomInfo() (err error) {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.getRoomInfo")
	var (
		req  *http.Request
		resp *http.Response
		body []byte
	)
	req, err = http.NewRequest("GET", fmt.Sprintf("https://m.huya.com/%s", l.rid), nil)
	if err != nil {
		return fmt.Errorf("making request for get room info error: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", global.DEFAULT_MOBILE_USER_AGENT)
	resp, err = l.client.Do(req)
	if err != nil {
		return fmt.Errorf("sending request for get room info error: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			log.Fatalln(err.Error())
		}
	}(resp.Body)
	body, err = io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	re := regexp.MustCompile(`<script> window.HNF_GLOBAL_INIT = ((.|\n)*) </script>`)
	result := re.FindStringSubmatch(string(body))
	if len(result) < 2 {
		return errors.New("HNF_GLOBAL_INIT not found")
	}
	log.WithField("HNF_GLOBAL_INIT", result[1]).Debugln("extract room info")
	vm := goja.New()
	_, err = vm.RunString(fmt.Sprintf(`
function init(){
	var obj = %s;
	var json = JSON.stringify(obj, function(key, value) {
  		if (typeof value === "function") {
    		return "/Function(" + value.toString() + ")/";
		}
  		return value;
	});
	var obj2 = JSON.parse(json, function(key, value) {
  		if (typeof value === "string" &&
      		value.startsWith("/Function(") &&
      		value.endsWith(")/")) {
    		value = value.substring(10, value.length - 2);
    		return (0, eval)("(" + value + ")");
  		}
  		return value;
	});
	return JSON.stringify(obj2)
};
`, result[1]))
	if err != nil {
		return fmt.Errorf("getting js result error: %w", err)
	}
	jsinit, ok := goja.AssertFunction(vm.Get("init"))
	if !ok {
		return fmt.Errorf("js init not found")
	}

	res, err := jsinit(goja.Undefined())
	if err != nil {
		return fmt.Errorf("getting js result error: %w", err)
	}
	log.WithField("data", res.Export().(string)).Debugln("extract room info")

	l.res = gjson.Parse(res.Export().(string))
	return nil
}

func (l *Link) getUUID() {
	log := global.Log.WithField("func", "app.engine.extractor.HuYa.getUUID")
	now := time.Now().UnixMilli()
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	random := int64(r.Intn(1000-0)+0) | 0
	l.uuid = strconv.FormatInt((now%10000000000*1000+random)%4294967295, 10)
	log.WithField("uuid", l.uuid).Debugln("generated UUID")
}
