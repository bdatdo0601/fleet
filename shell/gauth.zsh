# Source from ~/.zshrc on a Mac you log into remotely.
# `gauth` runs both gcloud logins in a tmux session; re-running reattaches, so a
# dropped mobile connection doesn't kill the waiting login.
gauth() {
  tmux new-session -A -s gauth \
    'gcloud auth login --no-launch-browser \
       && gcloud auth application-default login --no-launch-browser; \
     echo; gcloud auth list; exec zsh'
}
