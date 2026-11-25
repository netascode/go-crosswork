## 0.1.3 (unreleased)

- Write to the remote authentication servers and the SSO service providers under a lock of their own
- Share one wait for the login to recover between those writes
- Retry logins while the authentication service of Crosswork restarts
- Hold other requests on the client while the authentication service restarts
- Increase the default wait for logins after a change that restarts the authentication service (`authChangeWait`) from 180 to 300 seconds
- Add the texts of the refused rows (`failedData`) to the failure message of an Optima bulk write

## 0.1.2

- Add `Client.WaitForLogin` to wait until Crosswork accepts logins again after a change that restarts the authentication service
- Wait for logins to recover after a PUT to the remote authentication servers and the SSO service providers
- Increase the default wait for an NSO plan (`DefaultDefaultMaxAsyncWaitTime`) from 180 to 300 seconds

## 0.1.1

- Improve the documentation comments of the exported API

## 0.1.0

- Initial release
