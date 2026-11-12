// Package testaudio 提供完全合成的音频，只供隔离测试使用。
package testaudio

import (
	_ "embed"
	"encoding/binary"
	"math"
)

//go:embed tone.flac
var FLAC []byte

//go:embed tone.mp3
var MP3 []byte

// WAV 返回一秒钟 440Hz、8kHz、16bit 单声道合成 PCM，不包含真实歌曲。
func WAV() []byte { return Tone(1) }

// Tone 返回指定秒数的同一合成音频，用于时长需要与歌曲元数据相符的测试。
func Tone(seconds int) []byte {
	const sampleRate = 8000
	samples := sampleRate * seconds
	data := make([]byte, 44+samples*2)
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
	for i := range samples {
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(int16(math.Sin(float64(i)*2*math.Pi*440/sampleRate)*1000)))
	}
	return data
}
