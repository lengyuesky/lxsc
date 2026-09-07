// 浏览器验收专用：临时数据库、合成 SDK 和本地 WAV 上游，不接触生产数据或外部音乐资源。
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"lxsc/internal/admin"
	"lxsc/internal/assets"
	"lxsc/internal/db"
	"lxsc/internal/js"
	"lxsc/internal/logbuf"
	"lxsc/internal/music"
	"lxsc/internal/portal"
	"lxsc/internal/secret"
	"lxsc/internal/settings"
	"lxsc/internal/subsonic"
	"lxsc/internal/webauth"
)

const sdk = `
function song(source, key, query) {
  const names=['测试歌曲一','第二首歌曲','<img src=x onerror=alert(1)>'];
  return {source,songmid:String(key),name:(query ? query+' · ' : '')+names[Number(String(key).replace(/^fail-/,''))-1],singer:'测试歌手',albumName:'合成音频',interval:'00:45',types:[{type:'320k'},{type:'128k'}]};
}
globalThis.__sdk_call=(path,args)=> {
  if (path.endsWith('.leaderboard.getBoards')) return Promise.resolve({list:[
    {id:'旧标识',bangid:'hot',name:'热歌榜'},
    {bangid:'new',name:'新歌榜'},
    {bangid:'special',name:'<img src=x onerror=alert(1)>'},
  ]});
  return Promise.resolve({list:args[0]==='空' ? [] : [1,2,3].map(id=>song(path.split('.')[0],args[0]==='播放失败' ? 'fail-'+id : id,args[0]))});
};
globalThis.__sdk_info=(source,key)=>key==='missing' ? Promise.reject(new Error('测试缺失')) : Promise.resolve(song(source,key,''));
`

func wave() []byte {
	const sampleRate, seconds = 8000, 45
	data := make([]byte, 44+sampleRate*seconds*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 16)
	binary.LittleEndian.PutUint16(data[20:], 1)
	binary.LittleEndian.PutUint16(data[22:], 1)
	binary.LittleEndian.PutUint32(data[24:], sampleRate)
	binary.LittleEndian.PutUint32(data[28:], sampleRate*2)
	binary.LittleEndian.PutUint16(data[32:], 2)
	binary.LittleEndian.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:], uint32(len(data)-44))
	for i := range sampleRate * seconds {
		value := int16(math.Sin(float64(i)*2*math.Pi*440/sampleRate) * 1000)
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(value))
	}
	return data
}

func run() error {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "lxsc-browser-data-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	database, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		return err
	}
	defer database.Close()
	box, err := secret.Load(dir, "browser-fixture-key")
	if err != nil {
		return err
	}
	users := map[string]*db.User{}
	for _, name := range []string{"admin", "alice", "bob"} {
		password, err := box.Encrypt("test-password")
		if err != nil {
			return err
		}
		user, err := database.CreateUser(ctx, name, password, name == "admin", "320k")
		if err != nil {
			return err
		}
		users[name] = user
	}
	store, err := settings.New(ctx, database)
	if err != nil {
		return err
	}
	if _, err := store.Update(ctx, map[string]json.RawMessage{"showBoards": json.RawMessage(`false`)}); err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	prelude, err := assets.JS.ReadFile("js/prelude.js")
	if err != nil {
		return err
	}
	client := &http.Client{}
	pool, err := js.NewSDKPool(2, string(prelude), sdk, client, client, log)
	if err != nil {
		return err
	}
	defer pool.Close()
	audio := wave()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Query().Get("id"), "fail-") {
			http.Error(w, "测试直连受限", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "audio/wav")
		http.ServeContent(w, r, "tone.wav", time.Unix(0, 0), bytes.NewReader(audio))
	}))
	defer upstream.Close()
	sources := js.NewSourceManager(string(prelude), client, client, log)
	defer sources.UnloadAll()
	script := fmt.Sprintf(`lx.on(lx.EVENT_NAMES.request, ({info}) => Promise.resolve(%q+'/tone.wav?id='+info.musicInfo.songmid+'&quality='+info.type));
lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['128k','320k']},tx:{name:'测试',type:'music',actions:['musicUrl'],qualitys:['128k','320k']}}});`, upstream.URL)
	if _, err := sources.Load(ctx, 1, 1, script); err != nil {
		return err
	}
	catalog := music.NewCatalog(database, pool, sources, store, log)
	infos := catalog.Search(ctx, "初始", music.SearchOptions{Sources: []string{"wy"}, Limit: 3})
	if len(infos) != 3 {
		return fmt.Errorf("测试 SDK 未返回歌曲")
	}
	if err := catalog.RememberSync(ctx, infos); err != nil {
		return err
	}
	for _, item := range []struct {
		id, user, name string
		public         bool
		tracks         []string
	}{
		{"pl-alice", "alice", "Alice 歌单", false, []string{"tr-wy-1"}},
		{"pl-bob-public", "bob", "Bob 公开歌单", true, []string{"tr-wy-2", "tr-wy-missing"}},
		{"pl-bob-private", "bob", "Bob 私有歌单", false, nil},
	} {
		if err := database.CreatePlaylistFull(ctx, item.id, users[item.user].ID, item.name, "", item.public, item.tracks); err != nil {
			return err
		}
	}
	auth := &webauth.Manager{DB: database, Secret: box}
	authServer := &webauth.Server{Auth: auth, Settings: store}
	media := &subsonic.Server{DB: database, Catalog: catalog, Settings: store, Secret: box, Log: log, HTTP: client}
	app := &portal.Server{DB: database, Catalog: catalog, Settings: store, Secret: box, Log: log, Auth: auth, Stream: media.ServeWebStream}
	management := &admin.Server{DB: database, Catalog: catalog, Settings: store, Secret: box, Log: log, Auth: auth, Sources: sources, Logs: logbuf.New(100), HTTP: client, Version: "浏览器测试", StartAt: time.Now()}
	web, err := fs.Sub(assets.Web, "web")
	if err != nil {
		return err
	}
	r := chi.NewRouter()
	r.Mount("/api/auth", authServer.Routes())
	r.Mount("/api/app", app.Routes())
	r.Mount("/api/admin", management.Routes())
	r.Mount("/rest", media.Routes())
	r.Handle("/*", http.FileServer(http.FS(web)))
	server := httptest.NewServer(r)
	defer server.Close()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"url": server.URL, "upstreamURL": upstream.URL})
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(stop)
	<-stop
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
