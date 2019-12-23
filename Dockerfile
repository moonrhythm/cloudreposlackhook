FROM gcr.io/moonrhythm-containers/go-scratch

ADD cloudreposlackhook /cloudreposlackhook

CMD ["./cloudreposlackhook"]
