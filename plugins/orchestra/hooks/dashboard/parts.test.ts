import { expect, test } from 'claude-code/testing'
import type { ToolCallResult } from 'claude-code'
import { identityOf, pageOf, publishedUrlOf, publishPrompt } from './parts.ts'

const published = { result: { url: 'https://claude.ai/artifact/x' } } as unknown as ToolCallResult

test('publishedUrlOf matches only the board page file name', () => {
    expect(publishedUrlOf({ action: 'publish', file_path: '/sp/orchestra-dashboard.html' }, published))
        .toBe('https://claude.ai/artifact/x')
    expect(publishedUrlOf({ file_path: 'orchestra-dashboard.html' }, published))
        .toBe('https://claude.ai/artifact/x')
    expect(publishedUrlOf({ action: 'publish', file_path: '/sp/my-orchestra-dashboard.html' }, published))
        .toBeUndefined()
})

test('publishedUrlOf ignores asset uploads, other actions and refused or failed calls', () => {
    const file_path = '/sp/orchestra-dashboard.html'

    expect(publishedUrlOf({ action: 'publish', file_path, asset: true }, published)).toBeUndefined()
    expect(publishedUrlOf({ action: 'read', file_path }, published)).toBeUndefined()
    expect(publishedUrlOf({ action: 'publish', file_path }, { deny: 'no' } as ToolCallResult)).toBeUndefined()
    expect(publishedUrlOf({ action: 'publish', file_path }, { ...published, isError: true } as ToolCallResult))
        .toBeUndefined()
})

test('identityOf reads an empty account as none and rejects answers without a host', () => {
    expect(identityOf('{"account":"","host":"mbp"}')).toEqual({ account: null, host: 'mbp' })
    expect(identityOf('{"account":"","host":null}')).toBeUndefined()
    expect(identityOf('{"account":""}')).toBeUndefined()
    expect(identityOf('oops')).toBeUndefined()
})

test('pageOf keeps a non-string account as none', () => {
    expect(pageOf({ url: 'u', account: 3 })).toEqual({ url: 'u', account: null })
    expect(pageOf({ url: '' })).toBeUndefined()
})

test('publishPrompt names the copy source and target, and pins only a new page', () => {
    const fresh = publishPrompt('/root')
    const update = publishPrompt('/root', 'https://claude.ai/artifact/x')

    for (const prompt of [fresh, update]) {
        expect(prompt).toContain('/root/dashboard/dashboard.html')
        expect(prompt).toContain('orchestra-dashboard.html')
        expect(prompt).toContain('capabilities {"db": {}}')
    }
    expect(fresh).toContain('pin true')
    expect(update).not.toContain('pin true')
})
