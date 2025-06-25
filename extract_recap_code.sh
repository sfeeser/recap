#!/usr/bin/env bash

# This script extracts Go and HTML files from a combined input stream (like the Canvas document)
# and places them into their correct directory structure, relative to the current directory.
#
# This version is highly robust against unexpected HTML-like tags (e.g., <selection-tag>)
# that might be present in the source due to the environment from which content is copied.
# It filters these out BEFORE processing them.
#
# Usage:
# 1. Copy the entire content of the "RECAP Go Backend Codebase" Canvas document.
# 2. Save this script as, e.g., 'extract_recap_code.sh' and make it executable: chmod +x extract_recap_code.sh
# 3. Run the script, piping the copied content to it:
#    On macOS:  pbpaste | ./extract_recap_code.sh
#    On Linux:  xclip -selection clipboard -o | ./extract_recap_code.sh
#    (You might need to install 'xclip' if you don't have it: sudo apt-get install xclip)
#    Alternatively, paste the content into a temporary file (e.g., canvas_content.txt) and run:
#    ./extract_recap_code.sh < canvas_content.txt

set -euo pipefail # Exit immediately if a command exits with a non-zero status, exit if unset variables, exit on pipefail

echo "Starting code extraction and directory creation..."
echo "Ensure this script is run from your project's root directory (e.g., ~/git/recap/)"

CURRENT_FILE=""
# Use a process substitution with grep -v to filter out problematic lines
# This ensures lines containing "<selection-tag>" never reach the 'while read' loop.
while IFS= read -r line; do
    # Remove any stray '```go' or '```' if they somehow got inside a section
    # This specifically removes the Go language markdown fences, which are only for display
    # in the original combined block.
    filtered_line=$(echo "$line" | sed -E 's/^(```go|```)$//g')

    # If the line became empty after filtering, skip it
    if [[ -z "$filtered_line" ]]; then
        continue
    fi

    # Check for Go file delimiter (e.g., // --- recap-server/path/to/file.go ---)
    if [[ "$filtered_line" =~ ^//\ ---[[:space:]]recap-server/([^[:space:]]+?\.go)[[:space:]]---[[:space:]]*$ ]]; then
        RELATIVE_PATH="${BASH_REMATCH[1]}"
        DIR_NAME=$(dirname "$RELATIVE_PATH")
        
        mkdir -p "$DIR_NAME"
        CURRENT_FILE="$RELATIVE_PATH"
        echo "Extracting: $CURRENT_FILE"
        echo "" > "$CURRENT_FILE" # Clear file content if it exists, prepare for new content
        continue
    fi

    # Check for HTML file delimiter (e.g., <!-- --- recap-server/path/to/file.html --- -->)
    # The regex for HTML lines is slightly different to match HTML comments
    if [[ "$filtered_line" =~ ^//\ ---[[:space:]]*recap-server/([^[:space:]]+\.html)[[:space:]]*---$ ]]; then
        RELATIVE_PATH="${BASH_REMATCH[1]}"
        DIR_NAME=$(dirname "$RELATIVE_PATH")
        
        mkdir -p "$DIR_NAME"
        CURRENT_FILE="$RELATIVE_PATH"
        echo "Extracting: $CURRENT_FILE"
        echo "" > "$CURRENT_FILE" # Clear file content if it exists, prepare for new content
        continue
    fi

    # If we are currently writing to a file, append the filtered line
    if [[ -n "$CURRENT_FILE" ]]; then
        echo "$filtered_line" >> "$CURRENT_FILE"
    fi

done < <(cat /dev/stdin | grep -vE '<(selection-tag|/selection-tag)>') # Filter BEFORE the while loop

echo "Extraction complete."
echo "You can now navigate into your project root (e.g., 'cd recap') and run 'go mod tidy' followed by 'go run main.go'."