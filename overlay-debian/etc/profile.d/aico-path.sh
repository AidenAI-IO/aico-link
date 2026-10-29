# Make Aico Link command-line tools available to root and ordinary login shells.
# Keep existing command precedence and avoid duplicates when sourced again.
case ":${PATH-}:" in
    *:/usr/lib/aico:*) ;;
    *) PATH="${PATH:+${PATH}:}/usr/lib/aico" ;;
esac
export PATH
