package music

import (
	"bytes"
	"compress/gzip"
	"testing"
)

const lxV2Sample = `{"type":"playList_v2","data":[
 {"id":"default","name":"list__name_default","list":[
   {"id":"20505418_B3A52A7A958BF0AED0EBFBA2E9A818B7","name":"晴天","singer":"周杰伦","source":"kg","interval":"04:29",
    "meta":{"songId":20505418,"albumName":"叶惠美","picUrl":"http://img/1.jpg","qualitys":[{"type":"128k","size":"4 MB","hash":"B3A52A7A958BF0AED0EBFBA2E9A818B7"}],"_qualitys":{"128k":{"size":"4 MB"}},"albumId":"966846","hash":"b3a52a7a958bf0aed0ebfba2e9a818b7"}},
   {"id":"tx_003xv4w313tZHV","name":"红尘客栈","singer":"周杰伦","source":"tx","interval":"04:34",
    "meta":{"songId":"003xv4w313tZHV","albumName":"十二新作","picUrl":"","qualitys":[],"_qualitys":{},"albumId":"003Ow85E3pnoqi","strMediaMid":"000d5VXa1mFK4G","id":5177680,"albumMid":"003Ow85E3pnoqi"}},
   {"id":"local_1","name":"本地","singer":"","source":"local","interval":"01:00","meta":{"filePath":"/x.mp3"}},
   {"id":"wy_1","name":"缺 ID","singer":"","source":"wy","meta":{}}
 ]},
 {"id":"tx_abc","name":"好听","source":"tx","sourceListId":"1","list":[
   {"id":"wy_2612429483","name":"真爱假说","singer":"A、B","source":"wy","interval":"02:46","meta":{"songId":2612429483,"albumName":"真爱假说","picUrl":"","qualitys":[{"type":"flac","size":"1"}],"_qualitys":{},"albumId":243472619}}
 ]}
]}`

func TestParseLXListV2(t *testing.T) {
	lists, err := ParseLXList([]byte(lxV2Sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 2 {
		t.Fatalf("期望 2 个列表，得到 %d", len(lists))
	}
	def := lists[0]
	if def.Name != "默认列表" || def.Original != 4 || len(def.Tracks) != 2 || def.Skipped != 2 {
		t.Fatalf("默认列表解析异常: %+v", def)
	}
	kg := def.Tracks[0]
	if kg.TrackID() != "tr-kg-B3A52A7A958BF0AED0EBFBA2E9A818B7" || kg.SongMid() != "20505418" || kg.Album() != "叶惠美" || kg.Img() != "http://img/1.jpg" || kg.Duration() != 269 {
		t.Fatalf("酷狗歌曲转换异常: %s", kg.JSON())
	}
	if q := kg.Qualities(); len(q) != 1 || q[0] != "128k" {
		t.Fatalf("音质解析异常: %v", q)
	}
	tx := def.Tracks[1]
	if tx.TrackID() != "tr-tx-003xv4w313tZHV" || tx.Raw["strMediaMid"] != "000d5VXa1mFK4G" || tx.Raw["albumMid"] != "003Ow85E3pnoqi" {
		t.Fatalf("QQ 歌曲转换异常: %s", tx.JSON())
	}
	if lists[1].Name != "好听" || lists[1].Source != "tx" || len(lists[1].Tracks) != 1 || lists[1].Tracks[0].TrackID() != "tr-wy-2612429483" {
		t.Fatalf("在线列表解析异常: %+v", lists[1])
	}
}

func TestParseLXListGzipAndLegacy(t *testing.T) {
	legacy := `{"type":"playList","data":{
	  "defaultList":{"list":[{"songmid":"1","name":"旧版","singer":"S","source":"wy","albumName":"AL","types":[{"type":"320k"}]}]},
	  "loveList":{"list":[]},
	  "userList":[{"id":"u1","name":"自建","list":[{"songmid":"2","name":"二","singer":"","source":"kw"}]}]
	}}`
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write([]byte(legacy))
	zw.Close()
	lists, err := ParseLXList(buf.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(lists) != 3 || lists[0].Name != "默认列表" || lists[1].Name != "我的收藏" || lists[2].Name != "自建" {
		t.Fatalf("旧版结构解析异常: %+v", lists)
	}
	if len(lists[0].Tracks) != 1 || lists[0].Tracks[0].TrackID() != "tr-wy-1" || lists[2].Tracks[0].TrackID() != "tr-kw-2" {
		t.Fatalf("旧版歌曲解析异常")
	}
}

func TestParseLXListErrors(t *testing.T) {
	for _, in := range []string{"", "not json", `{"type":"x","data":[]}`, `{"type":"playList_v2","data":[]}`, `{"foo":1}`} {
		if _, err := ParseLXList([]byte(in)); err == nil {
			t.Errorf("输入 %q 应当报错", in)
		}
	}
}
