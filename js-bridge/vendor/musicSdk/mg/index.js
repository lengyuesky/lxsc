// lxsc 修改标记（2026-09-06 补记）：暴露专辑目录模块；详见 js-bridge/vendor/PATCHES.md。
// lxsc 修改（2026-10-09）：运行入口不再加载未接入的评论、热搜和搜索联想模块。
import { apis } from '../api-source' // 现在已适配服务器端
import leaderboard from './leaderboard'
import songList from './songList'
import musicSearch from './musicSearch'
import pic from './pic'
import lyric from './lyric'
import album from './album'

const mg = {
  songList,
  musicSearch,
  leaderboard,
  album,
  getMusicUrl(songInfo, type) {
    // 使用 api-source 获取 API（优先自定义源）
    return apis('mg').getMusicUrl(songInfo, type)
  },
  getLyric(songInfo) {
    return lyric.getLyric(songInfo)
  },
  getPic(songInfo) {
    return pic.getPic(songInfo)
  },
  getMusicDetailPageUrl(songInfo) {
    return `http://music.migu.cn/v3/music/song/${songInfo.copyrightId}`
  },
}

export default mg
