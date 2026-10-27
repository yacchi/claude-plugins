// The orchestra plugin's state contract: the values its hooks module keeps
// in $.state for the session (`claude plugin validate` checks them).
declare module 'claude-code' {
    interface PluginState {
        // Whether the band above the prompt shows its detail rows.
        orchestra: { bandOpen: boolean }
    }
}
