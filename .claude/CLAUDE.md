# Claude Plugins Marketplace

This repository is a Claude Code plugin marketplace that provides a curated collection of plugins.

## Project Structure

```
.
├── .claude-plugin/
│   └── marketplace.json    # Marketplace definition file
├── plugins/                # Plugin directories
│   └── <plugin-name>/
│       ├── plugin.json     # Plugin manifest
│       └── ...             # Plugin files (commands, agents, hooks, etc.)
├── .claude/
│   └── CLAUDE.md           # This file
└── README.md
```

## Development Guidelines

### Adding a New Plugin

1. Create a new directory under `plugins/<plugin-name>/`
2. Add a `plugin.json` manifest file with required fields
3. Add the plugin entry to `.claude-plugin/marketplace.json`
4. Test locally with `/plugin marketplace add ./`

### Plugin Manifest Schema

Each plugin should have a `plugin.json` with:
- `name`: Plugin identifier (kebab-case)
- `description`: Brief description of the plugin
- `version`: Semantic version
- `author`: Author information

### Marketplace Entry Schema

Add plugins to `marketplace.json` with:
- `name`: Plugin identifier
- `source`: Relative path to plugin directory (e.g., `./plugins/my-plugin`)
- `description`: Brief description
- `version`: Current version

## Commit Message Convention

Conventional Commits, **with the subject written in Japanese**:

```
<type>(<plugin-name>): <日本語の説明> (vX.Y.Z)
```

- `type`: `feat` / `fix` / `docs` / `chore` (also `refactor`, `test`, `perf`, `style`)
- Scope is the plugin directory name (`orchestra`, `compact-companion`, …). Omit it for
  marketplace-level changes — e.g. `feat: jobcan プラグインをマーケットプレイスに追加`.
- Append ` (vX.Y.Z)` when the change bumps a plugin's version. That version must
  be updated in **both** `plugins/<name>/.claude-plugin/plugin.json` and the
  plugin's entry in `.claude-plugin/marketplace.json`, in the same commit.
- The body (Japanese, a few lines) explains **why**, not what the diff already shows.

Examples from the history:

```
feat(orchestra): SessionEndでセッションのworktreeを後始末する (v0.26.0)
fix(orchestra): delegate経路のワーカーを隔離ツリーへ寄せる (v0.25.0)
```

A handful of early commits are in English; they predate this convention and are
not a precedent.

## Testing

Before submitting changes:
1. Validate marketplace JSON: `claude plugin validate .`
2. Test plugin installation locally