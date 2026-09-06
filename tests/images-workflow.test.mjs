import test from 'node:test'
import assert from 'node:assert/strict'
import fs from 'node:fs'

const read = relative => fs.readFileSync(new URL(`../${relative}`, import.meta.url), 'utf8')
const workflow = read('.github/workflows/images.yml')

test('所有第三方 Actions 均固定到已记录的官方仓库完整提交', () => {
  const pins = JSON.parse(read('.github/actions-pins.json'))
  const expected = new Map(pins.map(pin => [pin.repository, pin.commit]))
  const used = [...workflow.matchAll(/uses:\s+([^@\s]+)@([^\s]+)/g)]
  assert(used.length > 0)
  for (const [, repository, commit] of used) {
    assert.match(commit, /^[0-9a-f]{40}$/)
    assert.equal(commit, expected.get(repository), `${repository} 的固定提交未在官方核对记录中`)
  }
  assert.deepEqual([...new Set(used.map(match => match[1]))].sort(), [...expected.keys()].sort())
  assert.doesNotMatch(workflow, /pull_request_target|secrets\.(?!GITHUB_TOKEN)[A-Za-z_]+/)
})

test('PR 构建没有登录、推送或包写入权限，发布必须等待检查', () => {
  const buildOnly = workflow.split('\n  build-only:')[1].split('\n  publish:')[0]
  const publish = workflow.split('\n  publish:')[1]
  assert.match(buildOnly, /needs: check/)
  assert.match(buildOnly, /github\.event_name == 'pull_request'/)
  assert.match(buildOnly, /push: false/)
  assert.doesNotMatch(buildOnly, /login-action|packages: write|secrets\./)
  assert.match(publish, /needs: check/)
  assert.match(publish, /github\.event_name != 'pull_request'/)
  assert.match(publish, /packages: write/)
  assert.match(publish, /\$\{GITHUB_REPOSITORY,,\}/)
})

test('Docker 构建使用固定 Bun、严格锁文件、交叉编译并随附许可', () => {
  const dockerfile = read('Dockerfile')
  assert.match(dockerfile, /FROM --platform=\$BUILDPLATFORM oven\/bun:1\.3\.14 AS js/)
  assert.match(dockerfile, /RUN bun install --frozen-lockfile\n/)
  assert.doesNotMatch(dockerfile, /\|\|\s*bun install/)
  assert.match(dockerfile, /FROM --platform=\$BUILDPLATFORM golang:1\.27\.0-alpine AS build/)
  assert.match(dockerfile, /^FROM alpine:3\.24$/m)
  assert.match(dockerfile, /CGO_ENABLED=0 GOOS=\$\{TARGETOS\} GOARCH=\$\{TARGETARCH\}/)
  assert.match(dockerfile, /COPY LICENSE NOTICE THIRD_PARTY_NOTICES\.md \/usr\/share\/licenses\/lxsc\//)
  assert.match(dockerfile, /COPY licenses\/ \/usr\/share\/licenses\/lxsc\/licenses\//)
  assert.match(dockerfile, /cp \/lib\/apk\/db\/installed \/usr\/share\/licenses\/lxsc\/ALPINE_PACKAGES\.txt/)
  assert.doesNotMatch(dockerfile, /apk info --license/)
})

test('涉及 LGPL 的四个头文件翻译单元完整归档且没有函数实现', () => {
  const manifest = JSON.parse(read('licenses/manifest.json'))
  const libc = manifest.components.find(component => component.id === 'modernc.org/libc')
  const headers = libc.files.filter(file => file.path.endsWith('.go.txt'))
  assert.equal(headers.length, 4)
  for (const header of headers) {
    const source = read(header.path)
    assert.match(source, /GNU Lesser General Public/)
    assert.match(source, /version 2\.1 of the License/)
    assert.doesNotMatch(source, /^\s*func[\s(]/m)
  }
})
