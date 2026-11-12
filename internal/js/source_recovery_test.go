package js

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestQualityAliasesUseDeclaredScriptArgument(t *testing.T) {
	if chain := downgradeChain("flac24bit", []string{"hires", "flac", "192k", "128k"}); !reflect.DeepEqual(chain, []string{"hires", "flac", "192k", "128k"}) {
		t.Fatal(chain)
	}
	if chain := downgradeChain("hires", []string{"hires", "flac24bit", "320k"}); !reflect.DeepEqual(chain, []string{"flac24bit", "320k"}) {
		t.Fatal(chain)
	}
	for _, q := range []string{"hires", "192k"} {
		t.Run(q, func(t *testing.T) {
			m := newCallStatisticsManager(t)
			script := fmt.Sprintf(`lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:[%q]}}});globalThis.__lx_request=({info})=>{if(info.type!==%q)throw new Error('错误档位');return {url:'https://media.invalid/ready'}};`, q, q)
			if _, err := m.Load(context.Background(), 1, script); err != nil {
				t.Fatal(err)
			}
			r, err := m.MusicURL(context.Background(), "wy", nil, CanonicalQuality(q))
			if err != nil || r.Quality != CanonicalQuality(q) {
				t.Fatalf("%+v %v", r, err)
			}
		})
	}
}

func TestDenseFailureWindowHasFixedStart(t *testing.T) {
	g, now := newTestClock()
	for range 120 {
		g.record(1, "wy", "musicUrl", scheduleStrongFail, false, -1)
		*now = now.Add(time.Second)
	}
	state := g.states[scheduleKey{1, "wy", "musicUrl"}]
	if state.consec != 24 || !now.Before(state.cooldown) {
		t.Fatalf("密集失败未进入冷却: %d %v", state.consec, state.cooldown)
	}
}

func TestURLSuccessCannotClearMediaFailure(t *testing.T) {
	m := newCallStatisticsManager(t)
	g, now := newTestClock()
	m.schedule = g
	for range 3 {
		g.record(1, "wy", "musicUrl", scheduleSuccess, false, 10)
		m.ReportMediaOutcome(1, "wy", false)
		*now = now.Add(scheduleFailGrace + time.Second)
	}
	if state := g.states[scheduleKey{1, "wy", "media"}]; state.consec != 3 || !now.Before(state.cooldown) {
		t.Fatal(state)
	}
	m.ReportMediaOutcome(1, "wy", true, 30)
	if state := g.states[scheduleKey{1, "wy", "media"}]; state.consec != 0 || !state.cooldown.IsZero() {
		t.Fatal(state)
	}
}

func TestStructuredErrorsStopSourceWideDowngrade(t *testing.T) {
	for _, tc := range []struct {
		code string
		want error
	}{{"EAUTH", ErrSourceAuth}, {"ERATELIMIT", ErrSourceRateLimited}, {"EUPSTREAM", ErrSourceUnavailable}, {"EINVALIDRESPONSE", ErrInvalidSourceResponse}} {
		{
			t.Run(tc.code, func(t *testing.T) {
				m := newCallStatisticsManager(t)
				script := fmt.Sprintf(`lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['musicUrl'],qualitys:['flac','320k','128k']}}});globalThis.__lx_request=()=>Promise.reject(Object.assign(new Error('不得导出的错误'),{code:%q,retryAfter:60}));`, tc.code)
				if _, err := m.Load(context.Background(), 1, script); err != nil {
					t.Fatal(err)
				}
				_, err := m.MusicURL(context.Background(), "wy", nil, "flac")
				if !errors.Is(err, tc.want) || m.CallStatisticsOf(1, time.Now(), time.Hour).Total.Attempts != 1 {
					t.Fatal(err)
				}
				if tc.code == "ERATELIMIT" {
					_, _ = m.MusicURL(context.Background(), "wy", nil, "flac")
					if m.CallStatisticsOf(1, time.Now(), time.Hour).Total.Attempts != 1 {
						t.Fatal("退避期间重复调用")
					}
				}
			})
		}
	}
}

func TestLyricAndPicErrorsRemainStrongFailures(t *testing.T) {
	m := newCallStatisticsManager(t)
	script := `lx.send(lx.EVENT_NAMES.inited,{status:true,sources:{wy:{type:'music',actions:['lyric','pic'],qualitys:['320k']}}});globalThis.__lx_request=()=>{throw new Error('合成失败')};`
	if _, err := m.Load(context.Background(), 1, script); err != nil {
		t.Fatal(err)
	}
	_, _ = m.Lyric(context.Background(), "wy", nil)
	_, _ = m.Pic(context.Background(), "wy", nil)
	for _, action := range []string{"lyric", "pic"} {
		if m.schedule.states[scheduleKey{1, "wy", action}].consec != 1 {
			t.Fatal("错误被内部取消误分类", action)
		}
	}
}
