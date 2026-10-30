import type { ToolCallResult } from 'claude-code'

export const PAGE_FILE = 'orchestra-dashboard.html'
export type Identity = { account: string | null, host: string }
export type BoardPage = { url: string, account: string | null }

const isRecord = (value: unknown): value is Record<string, unknown> =>
    typeof value === 'object' && value !== null && !Array.isArray(value)

// `agent-exec status whoami` stdout → identity; the host is what makes it one.
export const identityOf = (stdout: string): Identity | undefined => {
    let parsed: unknown
    try {
        parsed = JSON.parse(stdout)
    } catch {
        return undefined
    }
    if (!isRecord(parsed) || typeof parsed.host !== 'string') {
        return undefined
    }

    return {
        account: typeof parsed.account === 'string' && parsed.account !== '' ? parsed.account : null,
        host: parsed.host,
    }
}

// The $.store value under "board"; a page without a URL is no page.
export const pageOf = (value: unknown): BoardPage | undefined => {
    if (!isRecord(value) || typeof value.url !== 'string' || value.url === '') {
        return undefined
    }

    return { url: value.url, account: typeof value.account === 'string' ? value.account : null }
}

export const publishPrompt = (root: string, url?: string): string => {
    const copy = `1. Copy ${root}/dashboard/dashboard.html unchanged into your scratchpad directory as ${PAGE_FILE} (one cp command).`
    // Without the declaration the page's claude.use("db") resolves null and it shows nothing.
    const capabilities = 'capabilities {"db": {}}'

    if (url === undefined) {
        return [
            'Publish the orchestra board page for this Claude account. Do exactly this and nothing else:',
            copy,
            `2. Call the Artifact tool once: action "publish", file_path set to that copy, ${capabilities}, title "orchestra", icon "chart", pin true. I want it pinned to my claude.ai sidebar so I can open it from my phone.`,
            '3. Reply with the page\'s URL only. Do not call ArtifactData: the orchestra plugin records the URL and fills the page itself.',
        ].join('\n')
    }

    return [
        `Update my orchestra board page at ${url} to the plugin's current version. Do exactly this and nothing else:`,
        copy,
        `2. Call the Artifact tool with action "read" and url "${url}", then with action "publish", url "${url}", file_path set to that copy and ${capabilities}.`,
        '3. Reply with the page\'s URL only. Do not call ArtifactData: the orchestra plugin fills the page itself.',
    ].join('\n')
}

// The published URL when this Artifact call published the board page.
export const publishedUrlOf = (
    input: Readonly<Record<string, unknown>>,
    answer: ToolCallResult,
): string | undefined => {
    if (input.action !== undefined && input.action !== 'publish') {
        return undefined
    }
    if (input.asset === true) {
        return undefined
    }
    const path = input.file_path
    if (typeof path !== 'string' || path.split('/').pop() !== PAGE_FILE) {
        return undefined
    }

    const { deny, isError, result } = answer as { deny?: unknown, isError?: unknown, result?: unknown }
    if (deny !== undefined || isError) {
        return undefined
    }
    if (!isRecord(result) || typeof result.url !== 'string' || result.url === '') {
        return undefined
    }

    return result.url
}
