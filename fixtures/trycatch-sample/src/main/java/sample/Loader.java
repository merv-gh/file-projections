package sample;

public class Loader {
    public String load(String path) {
        if (path == null) {
            return "no-path";
        }
        String data;
        try {
            data = read(path);
            data = data.trim();
        } catch (RuntimeException e) {
            data = "error:" + e.getMessage();
        } finally {
            audit(path);
        }
        return data;
    }

    String read(String path) {
        return "contents-of-" + path;
    }

    void audit(String path) {
        System.out.println("loaded " + path);
    }
}
