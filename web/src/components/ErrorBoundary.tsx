import { Component, type ErrorInfo, type ReactNode } from "react";

/**
 * Last line of defence for render-time exceptions.
 *
 * The SPA lives in an iframe on the webGUI page: an uncaught throw in any view
 * unmounts the whole React tree and leaves the user staring at an empty frame
 * with no hint that anything happened. A crash here is almost always a
 * malformed value from the daemon (a mime with no "/", a NaN size, an entry
 * shape the contract does not describe), so the panel names the error and
 * offers the one action that reliably recovers: reload the frame.
 */
interface Props {
  children: ReactNode;
}

interface State {
  error: Error | null;
}

export class ErrorBoundary extends Component<Props, State> {
  state: State = { error: null };

  static getDerivedStateFromError(error: Error): State {
    return { error };
  }

  componentDidCatch(error: Error, info: ErrorInfo): void {
    // Nothing to report to; the console is what a support request will paste.
    console.error("File Browser crashed while rendering:", error, info.componentStack);
  }

  private reload = () => {
    // The hash may itself be what triggered the crash, so drop back to the
    // default route rather than reloading straight into the same state.
    try {
      window.location.hash = "";
    } catch {
      /* ignore */
    }
    window.location.reload();
  };

  private dismiss = () => this.setState({ error: null });

  render(): ReactNode {
    const { error } = this.state;
    if (!error) return this.props.children;

    return (
      <div className="crash-panel" role="alert">
        <h1 className="crash-title">The file browser hit an unexpected error</h1>
        <p className="crash-body">
          Something in the page failed to render. Nothing on your server was changed — this plugin is read-only.
        </p>
        <pre className="crash-detail">{String(error?.message || error)}</pre>
        <div className="crash-actions">
          <button type="button" className="btn btn-primary" onClick={this.reload}>
            Reload
          </button>
          <button type="button" className="btn" onClick={this.dismiss}>
            Try to continue
          </button>
        </div>
      </div>
    );
  }
}
