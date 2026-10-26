import { expect, test } from 'claude-code/testing'
import { gitCall, redirections, segments, stripDataKeepingVars } from './shell.ts'

test('a redirection ampersand does not split the command', () => {
    expect(segments('ls x 2>&1 | tail -3')).toEqual(['ls x 2>&1 ', ' tail -3'])
    expect(segments('echo a >&2')).toEqual(['echo a >&2'])
    expect(segments('make 2>&1 && npm test &> log.txt')).toEqual(['make 2>&1 ', ' npm test &> log.txt'])
    expect(segments('sleep 1 & wait')).toEqual(['sleep 1 ', ' wait'])
    expect(redirections('npm test &> log.txt')).toEqual(['log.txt'])
    expect(redirections('ls x 2>&1')).toEqual([])
})

test('gitCall sees git past loop, group and wrapper words', () => {
    const cases: [string, { globals: string, verb: string, rest: string, viaXargs: boolean } | undefined][] = [
        ['do git merge -q --no-edit $b', { globals: '', verb: 'merge', rest: ' -q --no-edit $b', viaXargs: false }],
        [' then git -C /x reset --hard HEAD', { globals: ' -C /x', verb: 'reset', rest: ' --hard HEAD', viaXargs: false }],
        ['{ git rebase main', { globals: '', verb: 'rebase', rest: ' main', viaXargs: false }],
        ['(git rebase main)', { globals: '', verb: 'rebase', rest: ' main', viaXargs: false }],
        ['! git merge x', { globals: '', verb: 'merge', rest: ' x', viaXargs: false }],
        ['env GIT_EDITOR=true git merge x', { globals: '', verb: 'merge', rest: ' x', viaXargs: false }],
        ['FOO=1 time git merge x', { globals: '', verb: 'merge', rest: ' x', viaXargs: false }],
        [' xargs -n 1 git merge', { globals: '', verb: 'merge', rest: '', viaXargs: true }],
        ['done', undefined],
        ['echo git merge x', undefined],
        ['for b in x', undefined],
    ]
    for (const [segment, expected] of cases) {
        expect(gitCall(segment), segment).toEqual(expected)
    }
})

test('stripDataKeepingVars keeps quoted variable operands and strips data', () => {
    expect(stripDataKeepingVars('git merge "$b"')).toEqual('git merge $QUOTED')
    expect(stripDataKeepingVars('echo "plain text"')).toEqual('echo  ')
    expect(stripDataKeepingVars(`echo 'a "$b"'`)).toEqual('echo  ')
})
