import test from 'node:test'
import assert from 'node:assert/strict'
import { assertSameSet, inside, legalComments, parseGoList, sha256 } from '../scripts/licenses.mjs'

test('许可摘要逐字节区分原文与换行修改', () => {
  assert.equal(sha256('abc'), 'ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad')
  assert.notEqual(sha256('条款\n'), sha256('条款\r\n'))
})

test('许可副本只允许安全相对路径', () => {
  assert.equal(inside('/archive', 'go/LICENSE'), '/archive/go/LICENSE')
  for (const value of ['', '/etc/passwd', '../LICENSE', 'go/../../LICENSE', 'go\\..\\LICENSE']) {
    assert.throws(() => inside('/archive', value), /不安全/)
  }
})

test('解析双架构 go list 输出，保留嵌套模块信息', () => {
  assert.deepEqual(parseGoList('{\n\t"ImportPath": "a",\n\t"Module": {\n\t\t"Path": "b"\n\t}\n}\n{\n\t"ImportPath": "c"\n}\n'), [
    { ImportPath: 'a', Module: { Path: 'b' } }, { ImportPath: 'c' },
  ])
  assert.deepEqual(parseGoList(' \n'), [])
})

test('依赖或许可证集合增删都要求重新审核', () => {
  assertSameSet(['a', 'b', 'a'], ['b', 'a'], '示例')
  assert.throws(() => assertSameSet(['a', 'b'], ['a'], '示例'), /已变化/)
  assert.throws(() => assertSameSet(['a'], ['a', 'b'], '示例'), /已变化/)
  assert.throws(() => assertSameSet(['a@2'], ['a@1'], '示例'), /已变化/)
})

test('完整保留内联法律注释，不误收字符串内的示例', () => {
  const notice = '/*\nCopyright 示例作者\nPermission is hereby granted\n全部条款。\n*/'
  const lineNotice = '// Copyright 2026 示例作者\n// Redistribution and use\n// 完整免责声明。'
  const input = `${notice}\npackage p\nvar a = "// Copyright 假注释"\nvar b = \`/* Copyright 也不是注释 */\`\n${lineNotice}\nvar c = '\\''\n`
  assert.deepEqual(legalComments(input), [notice, lineNotice])
})

test('Go 通用头部由根 LICENSE 覆盖，但第三方作者声明不会被过滤', () => {
  const header = '// Copyright 2026 The Go Authors.\n// Use of this source code is governed by a BSD-style\n// license that can be found in the LICENSE file.'
  assert.deepEqual(legalComments(header), [])
  assert.deepEqual(legalComments(header.replace('The Go Authors.', 'Other Authors.')), [header.replace('The Go Authors.', 'Other Authors.')])
})
