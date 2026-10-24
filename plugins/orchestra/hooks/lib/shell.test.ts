import { expect, test } from 'claude-code/testing'
import { redirections, segments } from './shell.ts'

test('a redirection ampersand does not split the command', () => {
    expect(segments('ls x 2>&1 | tail -3')).toEqual(['ls x 2>&1 ', ' tail -3'])
    expect(segments('echo a >&2')).toEqual(['echo a >&2'])
    expect(segments('make 2>&1 && npm test &> log.txt')).toEqual(['make 2>&1 ', ' npm test &> log.txt'])
    expect(segments('sleep 1 & wait')).toEqual(['sleep 1 ', ' wait'])
    expect(redirections('npm test &> log.txt')).toEqual(['log.txt'])
    expect(redirections('ls x 2>&1')).toEqual([])
})
