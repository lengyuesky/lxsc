// lxsc 修改标记（2026-09-06 补记）：暴露歌手与专辑目录模块；详见 js-bridge/vendor/PATCHES.md。
// lxsc 修改（2026-10-09）：运行入口不再加载未接入的评论、热搜和搜索联想模块。
import leaderboard from './leaderboard'
import { apis } from '../api-source' // 现在已适配服务器端
import songList from './songList'
import musicSearch from './musicSearch'
import pic from './pic'
import lyric from './lyric'
import singer from './singer'
import album from './album'

const kg = {
  leaderboard,
  songList,
  musicSearch,
  singer,
  album,
  getMusicUrl(songInfo, type) {
    // 使用 api-source 获取 API（优先自定义源）
    return apis('kg').getMusicUrl(songInfo, type)
  },
  getLyric(songInfo) {
    return lyric.getLyric(songInfo)
  },
  // getLyric(songInfo) {
  //   return apis('kg').getLyric(songInfo)
  // },
  getPic(songInfo) {
    return pic.getPic(songInfo)
  },
  getMusicDetailPageUrl(songInfo) {
    return `https://www.kugou.com/song/#hash=${songInfo.hash}&album_id=${songInfo.albumId}`
  },
  // getPic(songInfo) {
  //   return apis('kg').getPic(songInfo)
  // },
}

export default kg
