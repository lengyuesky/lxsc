// lxsc 修改标记（2026-09-06 补记）：从封面字段而非对象本身替换图片尺寸；详见 js-bridge/vendor/PATCHES.md。
import { getMusicInfosByList } from './musicInfo'
import { createHttpFetch } from './util'

export default {
  /**
   * 获取歌手信息
   * @param {*} id
   */
  getInfo(id) {
    if (id == 0) throw new Error('歌手不存在') // kg源某些歌曲在歌手没被kg收录时返回的歌手id为0
    return createHttpFetch(`http://mobiles.kugou.com/api/v5/singer/info?singerid=${id}`).then(body => {
      if (!body) throw new Error('get singer info faild.')

      return {
        source: 'kg',
        id: body.singerid,
        info: {
          name: body.singername,
          desc: body.intro,
          avatar: body.imgurl.replace('{size}', 480),
          gender: body.grade === 1 ? 'man' : 'woman',
        },
        count: {
          music: body.songcount,
          album: body.albumcount,
        },
      }
    })
  },
  /**
   * 获取歌手专辑列表
   * @param {*} id
   * @param {*} page
   * @param {*} limit
   */
  getAlbumList(id, page = 1, limit = 10) {
    if (id == 0) throw new Error('歌手不存在')
    return createHttpFetch(`http://mobiles.kugou.com/api/v5/singer/album?singerid=${id}&page=${page}&pagesize=${limit}`).then(body => {
      if (!body.info) throw new Error('get singer album list faild.')

      const list = this.filterAlbumList(body.info)
      return {
        source: 'kg',
        list,
        limit,
        page,
        total: body.total,
      }
    })
  },
  /**
   * 获取歌手歌曲列表
   * @param {*} id
   * @param {*} page
   * @param {*} limit
   */
  async getSongList(id, page = 1, limit = 100) {
    if (id == 0) throw new Error('歌手不存在')
    const body = await createHttpFetch(`http://mobiles.kugou.com/api/v5/singer/song?singerid=${id}&page=${page}&pagesize=${limit}`)
    if (!body.info) throw new Error('get singer song list faild.')

    const list = await getMusicInfosByList(body.info)
    return {
      source: 'kg',
      list,
      limit,
      page,
      total: body.total,
    }
  },
  filterAlbumList(raw) {
    return raw.map(item => {
      return {
        id: item.albumid,
        count: item.songcount,
        info: {
          name: item.albumname,
          author: item.singername,
          img: (item.sizable_cover || item.imgurl || item.img || '').replaceAll('{size}', '480'),
          desc: item.intro,
        },
      }
    })
  },
}
