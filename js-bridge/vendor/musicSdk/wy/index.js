// lxsc 修改（2026-10-09）：运行入口不再加载未接入的评论、热搜和搜索联想模块。
import leaderboard from './leaderboard'
import { apis } from '../api-source'
import getLyric from './lyric'
import getMusicInfo from './musicInfo'
import musicSearch from './musicSearch'
import extendSearch from './extendSearch'
import extendDetail from './extendDetail'
import songList from './songList'

const wy = {
  leaderboard,
  musicSearch,
  extendSearch,
  extendDetail,
  songList,
  getMusicUrl(songInfo, type) {
    return apis('wy').getMusicUrl(songInfo, type)
  },
  getLyric(songInfo) {
    return getLyric(songInfo.songmid)
  },
  getPic(songInfo) {
    const requestObj = getMusicInfo(songInfo.songmid)
    return requestObj.promise.then(info => info.al.picUrl)
  },
  getMusicDetailPageUrl(songInfo) {
    return `https://music.163.com/#/song?id=${songInfo.songmid}`
  },
}

export default wy
