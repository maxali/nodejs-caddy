ARG NODE_VERSION=14

FROM node:${NODE_VERSION}-alpine

# Set the working directory
WORKDIR /app

# Copy the application code into the container
COPY ${APP_ENTRY_FILE} .

# Install dependencies (not needed for this example)
# RUN npm install --only=production

# Expose the application port
EXPOSE ${PORT}

# CMD ["sh", "-c", "echo ${APP_ENTRY_FILE}"]
# Start the application
ENTRYPOINT [ "sh -c", "${ENTRY_COMMAND}" "${APP_ENTRY_FILE}"]