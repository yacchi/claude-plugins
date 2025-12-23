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

Use Conventional Commits format in English:
- `feat:` for new features
- `fix:` for bug fixes
- `docs:` for documentation
- `chore:` for maintenance tasks

## Testing

Before submitting changes:
1. Validate marketplace JSON: `claude plugin validate .`
2. Test plugin installation locally