package docker

// Close releases this client's idle Unix-socket connections. The owner must
// cancel and join its active operations first; this never stops containers.
func (e *HTTPEngine) Close() error {
	if e != nil && e.client != nil {
		e.client.CloseIdleConnections()
	}
	return nil
}
