# Caddy Node.js Plugin

The Caddy Node.js Plugin allows you to run serverless Node.js applications with Caddy, by managing the lifecycle of your Node.js HTTP server instances. It starts a new server instance when needed, stops it after a specified idle timeout, and proxies incoming requests to the running instance.

It automatically starts and stops Node.js instances based on incoming requests and provides a simple configuration interface to manage your Node.js applications.

# Features

- Reverse proxy for Node.js applications
- Automatically starts a new Node.js HTTP server instance when needed
- Proxies incoming requests to the running Node.js server instance
- Stops the server instance after a specified idle timeout
- Support for multiple instances of Node.js applications with different configurations
- Lightweight and easy to integrate with existing Caddy configurations

# Prerequisites

- [Caddy v2](https://caddyserver.com/docs/install)

# Installation

To use the Caddy Node.js plugin, you need to build a custom Caddy binary with the plugin included. You can do this using the xcaddy tool:

- Install xcaddy if you haven't already:

```bash
go get -u github.com/caddyserver/xcaddy/cmd/xcaddy
```

- Build a custom Caddy binary with the Caddy Node.js plugin:

```bash
xcaddy build --with github.com/maxali/caddy-nodejs
```

This will create a new `caddy` binary in the current directory with the Node.js plugin included.

# Usage

In your Caddyfile, you can configure the Node.js plugin using the `nodejs` directive. The basic configuration requires the path to your Node.js application file and an optional port number.

Here's an example Caddyfile:

```
http://localhost:8080 {
	nodejs {
		file /path/to/your/nodejs/app.js
		port 3000
	}
}
```

In this example, the plugin will start a Node.js application at `/path/to/your/nodejs/app.js` on port 3000 and reverse proxy requests from `http://localhost:8080` to the Node.js application.

You can also configure multiple instances of Node.js applications, each with its own configuration:

```
http://localhost:8081 {
	nodejs {
		file /path/to/your/first/nodejs/app.js
		port 3001
	}
}

http://localhost:8082 {
	nodejs {
		file /path/to/your/second/nodejs/app.js
		port 3002
	}
}
```

In this example, two separate Node.js applications will be started and proxied to different ports.

# Configuration Options

The following options can be used within the `nodejs` directive in the Caddyfile:

- `file` (required): Path to the Node.js application file.
- `port` (optional): Port number for the Node.js application. Defaults to 3000 if not specified.

# Support and Contributions

If you encounter any issues or have feature requests, please open an issue on the [GitHub repository](https://github.com/maxali/caddy-nodejs). Contributions are welcome in the form of pull requests.

# License

This project is licensed under the [MIT License](https://opensource.org/licenses/MIT).
