package sample;

// Maps a score (0-100) to a label. Several branches; one condition is wrong.
public class Grader {
    public String of(int score) {
        if (score < 0 || score > 100) {
            return "invalid";
        }
        if (score >= 90) {
            return "A";
        }
        if (score > 60) {
            return "pass";
        }
        return "fail";
    }
}
