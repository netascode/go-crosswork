## 0.1.3 (unreleased)

- Write to the remote authentication servers and the SSO service providers under a lock of their own, so the restart of the authentication service that follows does not hold up other writes
- Share one wait for the login to recover between the writes of a burst to those paths, instead of one wait for each write
- Wait out the restart of the authentication service in the login calls (HTTP 500 "There was a problem proxying the request", or a request that is held) for up to 300 seconds; any other login failure is returned at once

## 0.1.2

- Add `Client.WaitForLogin` to wait until Crosswork accepts logins again after a change that restarts the authentication service
- Wait for logins to recover after a PUT to the remote authentication servers and the SSO service providers
- Increase the default wait for an NSO plan (`DefaultDefaultMaxAsyncWaitTime`) from 180 to 300 seconds

## 0.1.1

- Improve the documentation comments of the exported API

## 0.1.0

- Initial release
