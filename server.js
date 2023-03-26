const http = require("http");

const server = http.createServer((req, res) => {
  res.write("Hello");
  res.end(" World!");
});

console.log("server.js: PORT " + process.env.PORT)
server.listen(process.env.PORT || 9000, () => {
  console.log("Server listening on port " + process.env.PORT);
});
